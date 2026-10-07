package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web"
	"github.com/teddashh/AI-Intune/internal/web/terminalassets"
)

type boundaryAuthorizer struct {
	mu       sync.Mutex
	calls    []operatorauth.Permission
	decision operatorauth.Decision
	allow    bool
}

type boundaryAuthorizeFunc func(*http.Request, operatorauth.Permission) (*http.Request, operatorauth.Decision)

func (f boundaryAuthorizeFunc) Authorize(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision) {
	return f(r, permission)
}

const testOperatorAuthority = "100.64.200.2:8787"

func newBoundaryRequest(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Host = testOperatorAuthority
	return req
}

func (a *boundaryAuthorizer) Authorize(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision) {
	a.mu.Lock()
	a.calls = append(a.calls, permission)
	allow, decision := a.allow, a.decision
	a.mu.Unlock()
	if !allow {
		if decision.HTTPStatus == 0 {
			decision.HTTPStatus = http.StatusForbidden
		}
		if decision.Code == "" {
			decision.Code = operatorauth.CapabilityRequired
		}
		return nil, decision
	}
	principal := boundaryPrincipal(permission)
	return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
		Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized,
		Principal: principal, Detail: "authorized",
	}
}

func (a *boundaryAuthorizer) snapshotCalls() []operatorauth.Permission {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]operatorauth.Permission(nil), a.calls...)
}

func boundaryPrincipal(permission operatorauth.Permission) operatorauth.Principal {
	names, _ := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	principal := operatorauth.Principal{
		SourceAddr: "100.100.10.20", NodeStableID: "node-stable-1",
		DeviceName: "operator-laptop.example.ts.net.", TailnetUserID: "42",
		TailnetUserLogin: "operator@example.com", AuthMethod: operatorauth.AuthMethodLocalAPI,
		GrantedCapabilities: operatorauth.CapabilityNames{View: names.View, Operate: names.Operate, Admin: names.Admin},
	}
	switch permission {
	case operatorauth.View:
		principal.AuthorizedCapability = names.View
	case operatorauth.Operate:
		principal.AuthorizedCapability = names.Operate
	case operatorauth.Admin:
		principal.AuthorizedCapability = names.Admin
	}
	return principal
}

func boundaryStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOperatorRouteManifestMatchesAllRegisteredRoutes(t *testing.T) {
	st := boundaryStore(t)
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	h := &hub{store: st}
	nonOperatorMux := http.NewServeMux()
	nonOperatorRegistered := h.machineAndPublicRoutes(nonOperatorMux)
	nonOperatorRegistered = append(nonOperatorRegistered, registerAccountRoutes(nonOperatorMux, st, ui, testOperatorAuthority, clientip.Resolver{})...)
	operatorMux := http.NewServeMux()
	operatorRegistered := h.operatorRoutes(operatorMux)
	operatorRegistered = append(operatorRegistered, registerSecurityRoutes(operatorMux, st, testOperatorAuthority)...)
	operatorRegistered = append(operatorRegistered, ui.Routes(operatorMux)...)
	operatorRegistered = append(operatorRegistered, registerOperatorTerminalSocket(operatorMux, h, nil, testOperatorAuthority))
	if err := validateRouteManifests(nonOperatorRegistered, nonOperatorRoutePolicies,
		operatorRegistered, operatorRoutePolicies); err != nil {
		t.Fatal(err)
	}
	if len(nonOperatorRegistered) != 24 || len(nonOperatorRoutePolicies) != 24 {
		t.Fatalf("non-operator registered=%d policies=%d, want 24/24",
			len(nonOperatorRegistered), len(nonOperatorRoutePolicies))
	}
	if len(operatorRegistered) != 209 || len(operatorRoutePolicies) != 209 {
		t.Fatalf("operator registered=%d policies=%d, want 209/209",
			len(operatorRegistered), len(operatorRoutePolicies))
	}
	if len(nonOperatorRegistered)+len(operatorRegistered) != 233 {
		t.Fatalf("all registered routes=%d, 預期 233", len(nonOperatorRegistered)+len(operatorRegistered))
	}
	counts := map[operatorauth.Permission]int{}
	representations := map[operatorRepresentation]int{}
	profiles := map[operatorSecurityProfile]int{}
	for _, policy := range operatorRoutePolicies {
		counts[policy.Permission]++
		representations[policy.Representation]++
		profiles[policy.SecurityProfile]++
	}
	if counts[operatorauth.View] != 83 || counts[operatorauth.Operate] != 24 || counts[operatorauth.Admin] != 102 {
		t.Fatalf("permission counts=%v, want view=83 operate=24 admin=102", counts)
	}
	if representations[operatorJSON] != 107 || representations[operatorHTML] != 101 || representations[operatorPlain] != 1 {
		t.Fatalf("representation counts=%v, want JSON=107 HTML=101 plain=1", representations)
	}
	if profiles[operatorSecurityLocked] != 208 || profiles[operatorSecurityTerminal] != 1 {
		t.Fatalf("security profiles=%v, want locked=208 terminal=1", profiles)
	}
}

func TestNonOperatorRouteManifestValidationRejectsDriftAndOperatorShadowing(t *testing.T) {
	valid := []string{"GET /healthz"}
	policy := map[string]nonOperatorRoutePolicy{
		"GET /healthz": {nonOperatorHealth},
	}
	if err := validateNonOperatorRoutePolicies(valid, policy); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		registered []string
		policies   map[string]nonOperatorRoutePolicy
	}{
		{name: "registered route missing policy", registered: []string{"GET /future"}, policies: policy},
		{name: "policy missing route", registered: nil, policies: policy},
		{name: "duplicate route", registered: []string{"GET /healthz", "GET /healthz"}, policies: policy},
		{name: "invalid class", registered: valid, policies: map[string]nonOperatorRoutePolicy{"GET /healthz": {99}}},
		{name: "unknown method token", registered: []string{"BREW /v1/jobs"}, policies: map[string]nonOperatorRoutePolicy{"BREW /v1/jobs": {nonOperatorAgent}}},
		{name: "agent path outside v1", registered: []string{"POST /machines/x/retire"}, policies: map[string]nonOperatorRoutePolicy{"POST /machines/x/retire": {nonOperatorAgent}}},
		{name: "operator namespace root disguised as agent", registered: []string{"GET /v1/operator"}, policies: map[string]nonOperatorRoutePolicy{"GET /v1/operator": {nonOperatorAgent}}},
		{name: "operator API disguised as agent", registered: []string{"PUT /v1/operator/future"}, policies: map[string]nonOperatorRoutePolicy{"PUT /v1/operator/future": {nonOperatorAgent}}},
		{name: "escaped operator discriminator", registered: []string{"GET /v1/%6fperator/{rest...}"}, policies: map[string]nonOperatorRoutePolicy{"GET /v1/%6fperator/{rest...}": {nonOperatorAgent}}},
		{name: "variable plane can shadow operator API", registered: []string{"GET /v1/{plane}/machines/{id}/channel"}, policies: map[string]nonOperatorRoutePolicy{"GET /v1/{plane}/machines/{id}/channel": {nonOperatorAgent}}},
		{name: "catch all can shadow operator API", registered: []string{"GET /v1/{rest...}"}, policies: map[string]nonOperatorRoutePolicy{"GET /v1/{rest...}": {nonOperatorAgent}}},
		{name: "health alias", registered: []string{"GET /health"}, policies: map[string]nonOperatorRoutePolicy{"GET /health": {nonOperatorHealth}}},
		{name: "metrics alias", registered: []string{"GET /debug/metrics"}, policies: map[string]nonOperatorRoutePolicy{"GET /debug/metrics": {nonOperatorMetrics}}},
		{name: "metrics on the public mux", registered: []string{"GET /metrics"}, policies: map[string]nonOperatorRoutePolicy{"GET /metrics": {nonOperatorMetrics}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateNonOperatorRoutePolicies(test.registered, test.policies); err == nil {
				t.Fatal("invalid non-operator route manifest was accepted")
			}
		})
	}

	operatorPolicy := map[string]operatorRoutePolicy{
		"GET /healthz": {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	}
	if err := validateRouteManifests(valid, policy, valid, operatorPolicy); err == nil {
		t.Fatal("route shared by root and operator mux was accepted")
	}
}

func TestOperatorRouteManifestValidationRejectsDrift(t *testing.T) {
	valid := []string{"GET /{$}"}
	policy := map[string]operatorRoutePolicy{
		"GET /{$}": {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	}
	if err := validateOperatorRoutePolicies(valid, policy); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		registered []string
		policies   map[string]operatorRoutePolicy
	}{
		{name: "registered route missing policy", registered: []string{"GET /future"}, policies: policy},
		{name: "policy missing route", registered: nil, policies: policy},
		{name: "duplicate route", registered: []string{"GET /{$}", "GET /{$}"}, policies: policy},
		{name: "invalid permission", registered: valid, policies: map[string]operatorRoutePolicy{"GET /{$}": {99, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}}},
		{name: "invalid representation", registered: valid, policies: map[string]operatorRoutePolicy{"GET /{$}": {operatorauth.View, 99, operator.SourceKindWeb, operatorSecurityLocked}}},
		{name: "invalid source kind", registered: valid, policies: map[string]operatorRoutePolicy{"GET /{$}": {operatorauth.View, operatorHTML, "from-user-agent", operatorSecurityLocked}}},
		{name: "unsafe mutation classified view", registered: []string{"POST /future"}, policies: map[string]operatorRoutePolicy{"POST /future": {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}}},
		{name: "unknown method token", registered: []string{"BREW /future"}, policies: map[string]operatorRoutePolicy{"BREW /future": {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}}},
		{name: "operator API outside namespace", registered: valid, policies: map[string]operatorRoutePolicy{"GET /{$}": {operatorauth.View, operatorHTML, operator.SourceKindOperatorAPI, operatorSecurityLocked}}},
		{name: "omitted security profile", registered: []string{"GET /future"}, policies: map[string]operatorRoutePolicy{
			"GET /future": {Permission: operatorauth.View, Representation: operatorHTML, SourceKind: operator.SourceKindWeb},
		}},
		{name: "terminal document without its profile", registered: []string{operatorTerminalDocumentPattern}, policies: map[string]operatorRoutePolicy{
			operatorTerminalDocumentPattern: {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
		}},
		{name: "terminal profile on a second route", registered: []string{operatorTerminalDocumentPattern, "GET /other"}, policies: map[string]operatorRoutePolicy{
			operatorTerminalDocumentPattern: {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityTerminal},
			"GET /other":                    {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityTerminal},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateOperatorRoutePolicies(test.registered, test.policies); err == nil {
				t.Fatal("invalid route manifest was accepted")
			}
		})
	}
}

func TestEveryOperatorRouteRequestsItsExactPermission(t *testing.T) {
	st := boundaryStore(t)
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &boundaryAuthorizer{decision: operatorauth.Decision{
		HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired,
		Detail: "missing test capability",
	}}
	h := &hub{store: st}
	handler, err := newHubHTTPHandler(h, ui, authorizer, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	probeMux := http.NewServeMux()
	h.operatorRoutes(probeMux)
	registerSecurityRoutes(probeMux, st, testOperatorAuthority)
	ui.Routes(probeMux)
	registerOperatorTerminalSocket(probeMux, h, authorizer, testOperatorAuthority)
	tests := []struct {
		pattern string
		path    string
		policy  operatorRoutePolicy
	}{
		{"GET /metrics", "/metrics", operatorRoutePolicy{operatorauth.View, operatorPlain, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /{$}", "/", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /preferences/navigation-language", "/preferences/navigation-language", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines", "/machines", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/enrollment", "/machines/enrollment", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/enrollment/limit-preview", "/machines/enrollment/limit-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/enrollment/limits", "/machines/enrollment/limits", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /downloads/agent/{arch}", "/downloads/agent/amd64", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/lifecycle", "/machines/lifecycle", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/configuration", "/machines/configuration", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/compliance", "/machines/compliance", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/diagnostics", "/machines/diagnostics", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/{id}", "/machines/machine-1", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /jobs", "/jobs", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /jobs/{id}", "/jobs/job-1", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /apps", "/apps", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /apps/artifacts/{id}", "/apps/artifacts/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /apps/artifact-fetches/{id}", "/apps/artifact-fetches/operation-1", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /deployments", "/deployments", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /deployments/{id}", "/deployments/deploy-1", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /updates", "/updates", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/tickets", "/reports/tickets", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/tickets.csv", "/reports/tickets.csv", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/changes", "/reports/changes", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports", "/reports", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/{id}/timeline", "/machines/machine-1/timeline", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/{id}/timeline.csv", "/machines/machine-1/timeline.csv", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /audit", "/audit", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /settings/tailnet", "/settings/tailnet", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /tenant/maintenance", "/tenant/maintenance", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /tenant/data", "/tenant/data", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/{id}/data", "/machines/machine-1/data", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /machines/{id}/data.csv", "/machines/machine-1/data.csv", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /v1/operator/data-disclosure", "/v1/operator/data-disclosure", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/data", "/v1/operator/machines/machine-1/data", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /reports/enrollment", "/reports/enrollment", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/enrollment.csv", "/reports/enrollment.csv", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/software", "/reports/software", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/software.csv", "/reports/software.csv", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/install", "/reports/install", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/install.csv", "/reports/install.csv", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/profile", "/reports/profile", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /reports/profile.csv", "/reports/profile.csv", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /v1/operator/enrollment-report", "/v1/operator/enrollment-report", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/software-report", "/v1/operator/software-report", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/install-report", "/v1/operator/install-report", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/profile-report", "/v1/operator/profile-report", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/enrollment-limit", "/v1/operator/enrollment-limit", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/enrollment-limit/preview", "/v1/operator/enrollment-limit/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/enrollment-limit", "/v1/operator/enrollment-limit", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines", "/v1/operator/machines", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}", "/v1/operator/machines/machine-1", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/evidence", "/v1/operator/machines/machine-1/evidence", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/jobs", "/v1/operator/jobs", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/jobs/{id}", "/v1/operator/jobs/job-1", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/jobs/{id}/evidence", "/v1/operator/jobs/job-1/evidence", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/deployments", "/v1/operator/deployments", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/deployments/{id}", "/v1/operator/deployments/deployment-1", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/artifacts", "/v1/operator/artifacts", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/artifacts/{sha256}", "/v1/operator/artifacts/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/artifact-fetches/preview", "/v1/operator/artifact-fetches/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/artifact-fetches", "/v1/operator/artifact-fetches", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/artifact-fetches", "/v1/operator/artifact-fetches", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/artifact-fetches/{id}", "/v1/operator/artifact-fetches/operation-1", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/updates", "/v1/operator/updates", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/audit-events", "/v1/operator/audit-events", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/changes", "/v1/operator/changes", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/tickets", "/v1/operator/tickets", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/actions", "/v1/operator/machines/machine-1/actions", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/reports", "/v1/operator/reports", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/daily-report", "/v1/operator/daily-report", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/timeline", "/v1/operator/machines/machine-1/timeline", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/lifecycle", "/v1/operator/machines/machine-1/lifecycle", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/lifecycle-preview", "/v1/operator/machines/machine-1/lifecycle-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"PUT /v1/operator/machines/{id}/lifecycle", "/v1/operator/machines/machine-1/lifecycle", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/display-name-preview", "/v1/operator/machines/machine-1/display-name-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"PUT /v1/operator/machines/{id}/display-name", "/v1/operator/machines/machine-1/display-name", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/notes-preview", "/v1/operator/machines/machine-1/notes-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"PUT /v1/operator/machines/{id}/notes", "/v1/operator/machines/machine-1/notes", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/preview", "/v1/operator/deployments/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments", "/v1/operator/deployments", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/continuation-preview", "/v1/operator/deployments/deployment-1/continuation-preview", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/continuations", "/v1/operator/deployments/deployment-1/continuations", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/retry-preview", "/v1/operator/deployments/deployment-1/retry-preview", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/retries", "/v1/operator/deployments/deployment-1/retries", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/abandonment-preview", "/v1/operator/deployments/deployment-1/abandonment-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/abandonments", "/v1/operator/deployments/deployment-1/abandonments", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/channel", "/v1/operator/machines/machine-1/channel", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/assigned-user", "/v1/operator/machines/machine-1/assigned-user", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machines/{id}/enrollment-token", "/v1/operator/machines/machine-1/enrollment-token", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/diagnostic-noop-preview", "/v1/operator/machines/machine-1/diagnostic-noop-preview", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/diagnostic-noop-jobs", "/v1/operator/machines/machine-1/diagnostic-noop-jobs", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/catalog-manifests", "/v1/operator/catalog-manifests", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/catalog-manifests", "/v1/operator/catalog-manifests", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/machine-profiles", "/v1/operator/machine-profiles", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machine-profiles", "/v1/operator/machine-profiles", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/profile-assignment-preview", "/v1/operator/machines/machine-1/profile-assignment-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/profile-assignments", "/v1/operator/machines/machine-1/profile-assignments", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/catalog-manifests/standard-preview", "/v1/operator/catalog-manifests/standard-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machine-profiles/preview", "/v1/operator/machine-profiles/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/tailnet", "/v1/operator/tailnet", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/tailnet/peer-ignore-preview", "/v1/operator/tailnet/peer-ignore-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"PUT /v1/operator/tailnet/peer-ignores/{id}", "/v1/operator/tailnet/peer-ignores/node-1", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/maintenance/retention", "/v1/operator/maintenance/retention", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/maintenance/retention/prune-preview", "/v1/operator/maintenance/retention/prune-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/maintenance/retention/prunes", "/v1/operator/maintenance/retention/prunes", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/maintenance/restore-drill-preview", "/v1/operator/maintenance/restore-drill-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/maintenance/restore-drills", "/v1/operator/maintenance/restore-drills", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/maintenance/restore-drills", "/v1/operator/maintenance/restore-drills", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/maintenance/restore-drills/{id}", "/v1/operator/maintenance/restore-drills/operation-1", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /machines/{id}/connect", "/machines/machine-1/connect", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/artifact-fetches/preview", "/apps/artifact-fetches/preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/artifact-fetches", "/apps/artifact-fetches", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/store/packages/preview", "/apps/store/packages/preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/store/packages", "/apps/store/packages", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/profiles/preview", "/apps/profiles/preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/profiles", "/apps/profiles", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/profile-assignments/preview", "/apps/profile-assignments/preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /apps/profile-assignments", "/apps/profile-assignments", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /jobs/{id}/verifier-assignment-preview", "/jobs/job-1/verifier-assignment-preview", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /jobs/{id}/verifier-assignments", "/jobs/job-1/verifier-assignments", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/preview", "/deployments/preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments", "/deployments", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/continue-preview", "/deployments/deploy-1/continue-preview", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/continue", "/deployments/deploy-1/continue", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/retry-preview", "/deployments/deploy-1/retry-preview", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/retry", "/deployments/deploy-1/retry", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/abandon-preview", "/deployments/deploy-1/abandon-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/abandon", "/deployments/deploy-1/abandon", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/retire", "/machines/machine-1/retire", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/unretire", "/machines/machine-1/unretire", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/lifecycle-preview", "/machines/machine-1/lifecycle-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/configuration/policy-preview", "/machines/configuration/policy-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/configuration/policies", "/machines/configuration/policies", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/configuration/assignment-preview", "/machines/configuration/assignment-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/configuration/assignments", "/machines/configuration/assignments", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/compliance/policy-preview", "/machines/compliance/policy-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/compliance/policies", "/machines/compliance/policies", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/compliance/assignment-preview", "/machines/compliance/assignment-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/compliance/assignments", "/machines/compliance/assignments", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/diagnostic-noop-preview", "/machines/machine-1/diagnostic-noop-preview", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/diagnostic-noop-jobs", "/machines/machine-1/diagnostic-noop-jobs", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /enrollments", "/enrollments", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /enrollments/preview", "/enrollments/preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/revoke-token/preview", "/machines/machine-1/revoke-token/preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/keyed-installer", "/machines/machine-1/keyed-installer", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/revoke-token", "/machines/machine-1/revoke-token", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/channel", "/machines/machine-1/channel", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/assigned-user", "/machines/machine-1/assigned-user", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/assigned-user-preview", "/machines/machine-1/assigned-user-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/display-name-preview", "/machines/machine-1/display-name-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/display-name", "/machines/machine-1/display-name", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/notes-preview", "/machines/machine-1/notes-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /machines/{id}/notes", "/machines/machine-1/notes", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /settings/tailnet/peer-ignore-preview", "/settings/tailnet/peer-ignore-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /settings/tailnet/peer-ignores", "/settings/tailnet/peer-ignores", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /tenant/maintenance/retention/prune-preview", "/tenant/maintenance/retention/prune-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /tenant/maintenance/retention/prunes", "/tenant/maintenance/retention/prunes", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"GET /tenant/maintenance/restore-drills/{id}", "/tenant/maintenance/restore-drills/operation-1", operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /tenant/maintenance/restore-drill-preview", "/tenant/maintenance/restore-drill-preview", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /tenant/maintenance/restore-drills", "/tenant/maintenance/restore-drills", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"PUT /v1/operator/machines/{id}/channel", "/v1/operator/machines/machine-1/channel", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"PUT /v1/operator/machines/{id}/assigned-user", "/v1/operator/machines/machine-1/assigned-user", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/enrollment-tokens/preview", "/v1/operator/enrollment-tokens/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/enrollment-tokens", "/v1/operator/enrollment-tokens", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/enrollment-token/revocation-preview", "/v1/operator/machines/machine-1/enrollment-token/revocation-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/machines/{id}/enrollment-token/revocations", "/v1/operator/machines/machine-1/enrollment-token/revocations", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/verifiers/preview", "/v1/operator/verifiers/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/verifiers", "/v1/operator/verifiers", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/verifiers", "/v1/operator/verifiers", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/verifiers/{id}", "/v1/operator/verifiers/verifier-1", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/verifiers/{id}/revocation-preview", "/v1/operator/verifiers/verifier-1/revocation-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/verifiers/{id}/revocations", "/v1/operator/verifiers/verifier-1/revocations", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/verifiers/{id}/assignment-preview", "/v1/operator/verifiers/verifier-1/assignment-preview", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/verifiers/{id}/assignments", "/v1/operator/verifiers/verifier-1/assignments", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/settings", "/v1/operator/settings", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/setting-policies/preview", "/v1/operator/setting-policies/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/setting-policies", "/v1/operator/setting-policies", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/setting-assignments/preview", "/v1/operator/setting-assignments/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/setting-assignments", "/v1/operator/setting-assignments", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/compliance", "/v1/operator/compliance", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/compliance-policies/preview", "/v1/operator/compliance-policies/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/compliance-policies", "/v1/operator/compliance-policies", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/compliance-assignments/preview", "/v1/operator/compliance-assignments/preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/compliance-assignments", "/v1/operator/compliance-assignments", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /machines/{id}/terminals", "/machines/machine-1/terminals", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{operatorTerminalDocumentPattern, "/machines/machine-1/terminals/session-1", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityTerminal}},
		{operatorTerminalSocketPattern, "/machines/machine-1/terminals/session-1/socket", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/skip-failed-batch-preview", "/deployments/deploy-1/skip-failed-batch-preview", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /deployments/{id}/skip-failed-batch", "/deployments/deploy-1/skip-failed-batch", operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/skip-failed-batch-preview", "/v1/operator/deployments/deployment-1/skip-failed-batch-preview", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/deployments/{id}/skip-failed-batches", "/v1/operator/deployments/deployment-1/skip-failed-batches", operatorRoutePolicy{operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/disk-clean/summaries", "/v1/operator/disk-clean/summaries", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /v1/operator/disk-clean/summaries/{id}", "/v1/operator/disk-clean/summaries/machine-1", operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/profile-preview", "/v1/operator/disk-clean/profile-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/profiles", "/v1/operator/disk-clean/profiles", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/dry-run-preview", "/v1/operator/disk-clean/dry-run-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/dry-runs", "/v1/operator/disk-clean/dry-runs", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/canary-preview", "/v1/operator/disk-clean/canary-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/canaries", "/v1/operator/disk-clean/canaries", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/continuation-preview", "/v1/operator/disk-clean/continuation-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/continuations", "/v1/operator/disk-clean/continuations", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/abandonment-preview", "/v1/operator/disk-clean/abandonment-preview", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"POST /v1/operator/disk-clean/abandonments", "/v1/operator/disk-clean/abandonments", operatorRoutePolicy{operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}},
		{"GET /account/security", "/account/security", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /account/security/totp/begin", "/account/security/totp/begin", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /account/security/totp/confirm", "/account/security/totp/confirm", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /account/security/totp/disable", "/account/security/totp/disable", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		{"POST /account/security/password", "/account/security/password", operatorRoutePolicy{operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
	}
	casePatterns := make(map[string]struct{}, len(tests))
	for _, test := range tests {
		if _, duplicate := casePatterns[test.pattern]; duplicate {
			t.Fatalf("permission test contains duplicate route pattern %q", test.pattern)
		}
		casePatterns[test.pattern] = struct{}{}
		policy, ok := operatorRoutePolicies[test.pattern]
		if !ok {
			t.Fatalf("permission test names route %q absent from operatorRoutePolicies", test.pattern)
		}
		if policy != test.policy {
			t.Fatalf("route %q policy=%+v, test expects %+v", test.pattern, policy, test.policy)
		}
	}
	for pattern := range operatorRoutePolicies {
		if _, ok := casePatterns[pattern]; !ok {
			t.Fatalf("operatorRoutePolicies route %q has no exact permission test case", pattern)
		}
	}
	if len(casePatterns) != len(operatorRoutePolicies) {
		t.Fatalf("permission test patterns=%d policies=%d", len(casePatterns), len(operatorRoutePolicies))
	}
	for i, test := range tests {
		method, _, ok := strings.Cut(test.pattern, " ")
		if !ok {
			t.Fatalf("invalid test route pattern %q", test.pattern)
		}
		probe := newBoundaryRequest(method, test.path, nil)
		if _, matchedPattern := probeMux.Handler(probe); matchedPattern != test.pattern {
			t.Fatalf("%s concrete path %q matched %q", test.pattern, test.path, matchedPattern)
		}
		rec := httptest.NewRecorder()
		req := newBoundaryRequest(method, test.path, strings.NewReader("{}"))
		req.RemoteAddr = "100.100.10.20:4321"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s status=%d body=%s", test.pattern, rec.Code, rec.Body.String())
		}
		contentType := rec.Header().Get("Content-Type")
		if test.policy.Representation == operatorJSON && !strings.HasPrefix(contentType, "application/json") {
			t.Errorf("%s content-type=%q, want JSON", test.pattern, contentType)
		}
		if test.policy.Representation == operatorHTML && !strings.HasPrefix(contentType, "text/html") {
			t.Errorf("%s content-type=%q, want HTML", test.pattern, contentType)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s Cache-Control=%q", test.pattern, rec.Header().Get("Cache-Control"))
		}
		wantCSP := lockedOperatorContentSecurityPolicy
		if test.pattern == operatorTerminalDocumentPattern {
			wantCSP = terminalDocumentCSPForTest(false)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s CSP=%q, want %q", test.pattern, got, wantCSP)
		}
		calls := authorizer.snapshotCalls()
		if len(calls) != i+1 || calls[i] != test.policy.Permission {
			t.Fatalf("%s auth calls=%v, want final %s", test.pattern, calls, test.policy.Permission)
		}
	}
}

func TestMachineAndPublicRoutesNeverCallOperatorAuthorizer(t *testing.T) {
	st := boundaryStore(t)
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &boundaryAuthorizer{decision: operatorauth.Decision{
		HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired, Detail: "denied",
	}}
	handler, err := newHubHTTPHandler(&hub{store: st}, ui, authorizer, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/healthz"},
		{http.MethodPost, "/v1/enrollments"},
		{http.MethodPost, "/v1/checkins"},
		{http.MethodGet, "/v1/jobs/next"},
		{http.MethodGet, "/v1/agent/readiness"},
		{http.MethodGet, "/v1/capabilities"},
		{http.MethodGet, "/v1/artifacts/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	} {
		rec := httptest.NewRecorder()
		req := newBoundaryRequest(test.method, test.path, strings.NewReader("{}"))
		req.RemoteAddr = "100.100.10.20:4321"
		handler.ServeHTTP(rec, req)
	}
	if calls := authorizer.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("machine/public plane called human authorizer: %v", calls)
	}

	rec := httptest.NewRecorder()
	req := newBoundaryRequest(http.MethodGet, "/v1/operator/machines/nope/channel", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	req.Header.Set("Authorization", "Bearer machine-token-cannot-help")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(authorizer.snapshotCalls()) != 1 {
		t.Fatalf("machine bearer changed operator decision: status=%d calls=%v body=%s",
			rec.Code, authorizer.snapshotCalls(), rec.Body.String())
	}
}

func TestMetricsPinsLiteralAuthorityWithoutDependingOnOperatorAuth(t *testing.T) {
	st := boundaryStore(t)
	h := &hub{store: st}
	machineMux := http.NewServeMux()
	h.machineAndPublicRoutes(machineMux)
	machine := enrollViaHTTP(t, machineMux, st, "metrics-secret-machine")

	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	denied := &boundaryAuthorizer{decision: operatorauth.Decision{
		HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired,
		Detail: "view grant required",
	}}
	handler, err := newHubHTTPHandler(h, ui, denied, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}

	hostile := httptest.NewRequest(http.MethodGet, "http://evil.example:8787/metrics", nil)
	hostile.Host = "evil.example:8787"
	hostile.RemoteAddr = "100.100.10.20:4321"
	hostile.Header.Set("Origin", "http://evil.example:8787")
	hostile.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hostile)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("hostile Host metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), machine.id) || strings.Contains(rec.Body.String(), "metrics-secret-machine") {
		t.Fatalf("hostile Host leaked inventory: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("metrics rejection security headers=%v", rec.Header())
	}
	if calls := denied.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("hostile Host reached the authorizer: %v", calls)
	}

	rec = httptest.NewRecorder()
	req := newBoundaryRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing view grant status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), machine.id) || strings.Contains(rec.Body.String(), "metrics-secret-machine") {
		t.Fatalf("missing view grant leaked inventory: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("metrics denial content-type=%q", ct)
	}
	if calls := denied.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.View {
		t.Fatalf("missing view grant auth calls=%v", calls)
	}

	allowed := &boundaryAuthorizer{allow: true}
	handler, err = newHubHTTPHandler(h, ui, allowed, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = newBoundaryRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), machine.id) ||
		!strings.Contains(rec.Body.String(), "metrics-secret-machine") {
		t.Fatalf("view grant metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls := allowed.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.View {
		t.Fatalf("view grant auth calls=%v", calls)
	}

	// /healthz intentionally remains a content-free liveness probe under any
	// Host so load balancers do not acquire the operator auth failure domain.
	rec = httptest.NewRecorder()
	health := httptest.NewRequest(http.MethodGet, "http://evil.example:8787/healthz", nil)
	health.Host = "evil.example:8787"
	handler.ServeHTTP(rec, health)
	if rec.Code != http.StatusOK || rec.Body.String() != "alive\n" {
		t.Fatalf("healthz status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestDenialLimiterKeepsUnsafeAuditQuotaSeparateFromSafeLogQuota(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	limiter := newOperatorDenialLimiter(func() time.Time { return now })
	for i := 0; i < denialLogBurst+5; i++ {
		admission := limiter.admit(false, "safe-flood")
		if admission.persistUnsafe {
			t.Fatal("safe denial consumed unsafe persistent-audit quota")
		}
	}
	firstUnsafe := limiter.admit(true, "unsafe-first")
	if firstUnsafe.emitLog || !firstUnsafe.persistUnsafe {
		t.Fatalf("safe flood suppressed first unsafe audit: %+v", firstUnsafe)
	}
	for i := 1; i < denialAuditBurst; i++ {
		if admission := limiter.admit(true, "unsafe-sampled"); !admission.persistUnsafe {
			t.Fatalf("unsafe denial %d was not sampled: %+v", i+1, admission)
		}
	}
	if admission := limiter.admit(true, "unsafe-suppressed"); admission.persistUnsafe {
		t.Fatalf("unsafe audit exceeded fixed burst: %+v", admission)
	}

	now = now.Add(denialAuditWindow)
	rollover := limiter.admit(false, "next-window")
	if !rollover.emitLog || rollover.persistUnsafe || rollover.suppressedLogs != 18 ||
		rollover.suppressedUnsafe != 1 || rollover.lastUnsafeSource != "unsafe-suppressed" {
		t.Fatalf("rollover summary=%+v, want logs=18 unsafe=1 and fresh safe log", rollover)
	}
}

func TestSafeDenialFloodCannotSuppressFirstUnsafePersistentAudit(t *testing.T) {
	st := boundaryStore(t)
	inner := http.NewServeMux()
	inner.HandleFunc("GET /{$}", func(http.ResponseWriter, *http.Request) {})
	inner.HandleFunc("POST /mutate", func(http.ResponseWriter, *http.Request) {
		t.Fatal("host-denied request reached mutation")
	})
	policies := map[string]operatorRoutePolicy{
		"GET /{$}":     {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
		"POST /mutate": {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	}
	boundary := newOperatorBoundary(inner, &boundaryAuthorizer{allow: true}, st, policies, testOperatorAuthority)

	for i := 0; i < denialLogBurst+5; i++ {
		req := newBoundaryRequest(http.MethodGet, "/", nil)
		req.Host = "evil.example:8787"
		req.RemoteAddr = "100.100.10.20:4321"
		boundary.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := newBoundaryRequest(http.MethodPost, "/mutate", nil)
	req.Host = "evil.example:8787"
	req.RemoteAddr = "100.100.10.20:4321"
	rec := httptest.NewRecorder()
	boundary.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unsafe Host denial status=%d body=%s", rec.Code, rec.Body.String())
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 || entries[0].Action != store.AuditOperatorDenied ||
		entries[0].BoundaryDecision != operatorAuthorityDecisionCode {
		t.Fatalf("safe flood suppressed unsafe audit: entries=%+v err=%v", entries, err)
	}
}

func TestUnsafeDenialFloodIsSampledAndAggregatedAtRollover(t *testing.T) {
	st := boundaryStore(t)
	inner := http.NewServeMux()
	inner.HandleFunc("POST /mutate", func(http.ResponseWriter, *http.Request) {
		t.Fatal("host-denied request reached mutation")
	})
	inner.HandleFunc("GET /{$}", func(http.ResponseWriter, *http.Request) {})
	policies := map[string]operatorRoutePolicy{
		"POST /mutate": {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
		"GET /{$}":     {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	}
	boundary := newOperatorBoundary(inner, &boundaryAuthorizer{allow: true}, st, policies, testOperatorAuthority)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	boundary.denials = newOperatorDenialLimiter(func() time.Time { return now })
	for i := 0; i < denialAuditBurst+5; i++ {
		req := newBoundaryRequest(http.MethodPost, "/mutate", nil)
		req.Host = "evil.example:8787"
		req.RemoteAddr = "100.100.10.20:4321"
		boundary.ServeHTTP(httptest.NewRecorder(), req)
	}
	entries, err := st.Audit("", store.AuditLimit)
	if err != nil || len(entries) != denialAuditBurst+1 {
		t.Fatalf("sampled unsafe rows=%d, want %d err=%v", len(entries), denialAuditBurst+1, err)
	}
	for i := 0; i < 3; i++ {
		safe := newBoundaryRequest(http.MethodGet, "/", nil)
		safe.Host = "evil.example:8787"
		safe.RemoteAddr = "100.100.10.22:4321"
		boundary.ServeHTTP(httptest.NewRecorder(), safe)
	}

	now = now.Add(denialAuditWindow)
	rollover := newBoundaryRequest(http.MethodGet, "/", nil)
	rollover.Host = "evil.example:8787"
	rollover.RemoteAddr = "100.100.10.21:4321"
	boundary.ServeHTTP(httptest.NewRecorder(), rollover)
	entries, err = st.Audit("", store.AuditLimit)
	if err != nil || len(entries) != denialAuditBurst+2 {
		t.Fatalf("rows after aggregate=%d, want %d err=%v", len(entries), denialAuditBurst+2, err)
	}
	var aggregate *store.AuditEntry
	for i := range entries {
		if entries[i].BoundaryDecision == denialRateLimitedDecisionCode {
			aggregate = &entries[i]
			break
		}
	}
	if aggregate == nil || !strings.Contains(aggregate.Detail, "suppressed_unsafe_audit=5") {
		t.Fatalf("missing truthful denial aggregate: %+v", entries)
	}
	if got, want := aggregate.SourceAddr, "100.100.10.20"; got != want {
		t.Errorf("aggregate source got=%q, want=%q; the summary counts withheld write denials, so naming the last log-suppressed caller sends the operator to a source whose requests were all recorded", got, want)
	}
}

func TestUnsafeDenialFloodRecordsTruncationBeforeAnyoneElseArrives(t *testing.T) {
	st := boundaryStore(t)
	inner := http.NewServeMux()
	inner.HandleFunc("POST /mutate", func(http.ResponseWriter, *http.Request) {
		t.Error("host-denied request reached mutation")
	})
	policies := map[string]operatorRoutePolicy{
		"POST /mutate": {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	}
	boundary := newOperatorBoundary(inner, &boundaryAuthorizer{allow: true}, st, policies, testOperatorAuthority)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	boundary.denials = newOperatorDenialLimiter(func() time.Time { return now })
	const source = "100.100.10.20"
	for i := 0; i < denialAuditBurst+1; i++ {
		req := newBoundaryRequest(http.MethodPost, "/mutate", nil)
		req.Host = "evil.example:8787"
		req.RemoteAddr = source + ":4321"
		boundary.ServeHTTP(httptest.NewRecorder(), req)
	}

	entries, err := st.Audit("", store.AuditLimit)
	if err != nil {
		t.Errorf("audit read error got=%v, want=nil; without immediate visibility the audit can look like a complete stream", err)
	}
	if got, want := len(entries), denialAuditBurst+1; got != want {
		t.Errorf("audit rows got=%d, want=%d; readers could mistake %d sampled denials for the entire attack", got, want, denialAuditBurst)
	}
	truncations := 0
	var truncated store.AuditEntry
	for i := range entries {
		if entries[i].BoundaryDecision == denialAuditTruncatedDecisionCode {
			truncations++
			truncated = entries[i]
		}
	}
	if got, want := truncations, 1; got != want {
		t.Errorf("truncation rows got=%d, want=%d; the audit must disclose exactly once that its per-request stream became incomplete", got, want)
	}
	if truncations == 1 {
		if got, want := truncated.SourceAddr, source; got != want {
			t.Errorf("truncation source got=%q, want=%q; a wrong source misattributes who crossed the audit limit", got, want)
		}
		if strings.Contains(truncated.Detail, "suppressed_unsafe_audit=") {
			t.Errorf("truncation detail carries a settled count: %q; a count here reads as the attack total when it is only the first suppressed request",
				truncated.Detail)
		}
	}

	for i := 0; i < 3; i++ {
		req := newBoundaryRequest(http.MethodPost, "/mutate", nil)
		req.Host = "evil.example:8787"
		req.RemoteAddr = source + ":4321"
		boundary.ServeHTTP(httptest.NewRecorder(), req)
	}
	entries, err = st.Audit("", store.AuditLimit)
	if err != nil {
		t.Errorf("audit reread got=%v, want=nil; without a second read this test cannot tell whether the window wrote more than one marker", err)
	}
	truncations = 0
	for i := range entries {
		if entries[i].BoundaryDecision == denialAuditTruncatedDecisionCode {
			truncations++
		}
	}
	if got, want := len(entries), denialAuditBurst+1; got != want {
		t.Errorf("audit rows after further denials got=%d, want=%d; repeated markers would distort the apparent attack volume", got, want)
	}
	if got, want := truncations, 1; got != want {
		t.Errorf("truncation rows after further denials got=%d, want=%d; one window must have exactly one incompleteness marker", got, want)
	}
}

func TestDenialWindowRollsOverWhenHubClockMovesBackward(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	limiter := newOperatorDenialLimiter(func() time.Time { return now })
	for i := 0; i < denialAuditBurst; i++ {
		admission := limiter.admit(true, "flood")
		if got, want := admission.persistUnsafe, true; got != want {
			t.Errorf("unsafe admission %d persistUnsafe got=%t, want=%t; quota-fill precondition failed, so the rollover assertions are not meaningful", i+1, got, want)
		}
	}
	for i := 0; i < 2; i++ {
		admission := limiter.admit(true, "flood")
		if got, want := admission.persistUnsafe, false; got != want {
			t.Errorf("suppressed unsafe admission %d persistUnsafe got=%t, want=%t; suppression precondition failed, so the rollover assertions are not meaningful", i+1, got, want)
		}
	}

	now = now.Add(-30 * time.Second)
	rollover := limiter.admit(true, "after-rewind")
	if got, want := rollover.persistUnsafe, true; got != want {
		t.Errorf("rollover persistUnsafe got=%t, want=%t; if a clock rewind does not reset the quota, write denials stop receiving per-request audit records and the operator sees only one truncation row with no later window records", got, want)
	}
	if got, want := rollover.suppressedUnsafe, uint64(2); got != want {
		t.Errorf("rollover suppressedUnsafe got=%d, want=%d; unsettled denials never appear in the audit summary, so the operator undercounts the prior window", got, want)
	}
	if got, want := rollover.windowEnd, now; !got.Equal(want) {
		t.Errorf("rollover windowEnd got=%s, want=%s; the summary points to the wrong time range, so operators cannot correlate it with other records", got, want)
	}
	if got, want := rollover.auditTruncated, false; got != want {
		t.Errorf("rollover auditTruncated got=%t, want=%t; a fresh window marked truncated emits a false warning that per-request auditing stopped even though this denial was recorded", got, want)
	}
}

func TestDenialSummaryNamesTheSourceWhoseAuditWasWithheld(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	limiter := newOperatorDenialLimiter(func() time.Time { return now })
	for i := 0; i < denialAuditBurst; i++ {
		limiter.admit(true, "unsafe-recorded")
	}
	limiter.admit(true, "unsafe-withheld")
	for i := 0; i < denialLogBurst; i++ {
		limiter.admit(false, "safe-latecomer")
	}

	now = now.Add(denialAuditWindow)
	rollover := limiter.admit(false, "next-window")
	if got, want := rollover.lastUnsafeSource, "unsafe-withheld"; got != want {
		t.Errorf("rollover lastUnsafeSource got=%q, want=%q; the denial summary would name a source whose unsafe audit was not withheld", got, want)
	}
	if got, want := rollover.lastLogSource, "safe-latecomer"; got != want {
		t.Errorf("rollover lastLogSource got=%q, want=%q; later safe log suppression would not remain independently attributable", got, want)
	}
	if got, want := rollover.suppressedUnsafe, uint64(1); got != want {
		t.Errorf("rollover suppressedUnsafe got=%d, want=%d; the summary would misstate how many write-denial audit records were withheld", got, want)
	}
	if got := rollover.suppressedLogs; got == 0 {
		t.Errorf("rollover suppressedLogs got=%d, want>0; the test would not prove that a later safe denial can overwrite log-source state", got)
	}
}

func TestRegisteredButUnclassifiedRouteFailsClosed(t *testing.T) {
	st := boundaryStore(t)
	inner := http.NewServeMux()
	mutations := 0
	inner.HandleFunc("POST /future-control", func(http.ResponseWriter, *http.Request) { mutations++ })
	authorizer := &boundaryAuthorizer{allow: true}
	boundary := newOperatorBoundary(inner, authorizer, st, map[string]operatorRoutePolicy{}, testOperatorAuthority)
	rec := httptest.NewRecorder()
	req := newBoundaryRequest(http.MethodPost, "/future-control", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	boundary.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || mutations != 0 || len(authorizer.snapshotCalls()) != 0 {
		t.Fatalf("unclassified route did not fail closed: status=%d mutations=%d calls=%v",
			rec.Code, mutations, authorizer.snapshotCalls())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != lockedOperatorContentSecurityPolicy {
		t.Fatalf("unclassified route CSP=%q", got)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 || entries[0].Action != store.AuditOperatorDenied ||
		entries[0].BoundaryDecision != string(operatorauth.AuthConfigurationInvalid) ||
		entries[0].AuthDecision != "" {
		t.Fatalf("unclassified denial audit=%+v err=%v", entries, err)
	}
}

func TestContradictoryAuthorizationResultNeverReachesMutation(t *testing.T) {
	for _, test := range []struct {
		name       string
		target     string // 空字串代表 "/mutate"
		contract   string // 空字串代表沿用現有契約句
		reason     string // 期望出現在稽核 Detail 裡的那個常數
		authorizer boundaryAuthorizeFunc
	}{
		{
			name:   "allowed without principal",
			reason: string(operatorAuthSuccessPrincipalMissing),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				return r, operatorauth.Decision{Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized}
			},
		},
		{
			name:   "allowed with wrong exact capability",
			reason: string(operatorAuthSuccessCapabilityDiffers),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.View)
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed with incomplete request",
			reason: string(operatorAuthSuccessRequestIncomplete),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL = nil
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed after in-place path rewrite",
			reason: string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.Path = "/other"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed after in-place query rewrite",
			reason: string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.RawQuery = "scope=all"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed after method rewrite",
			reason: string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.Method = http.MethodGet
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed after host rewrite",
			reason: string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.Host = "hub.example.invalid:8787"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed after force query rewrite",
			target:   "/mutate?",
			contract: "adapter 把 ForceQuery 由 true 改成 false、RawQuery 仍空時，GET /…? 會通過那條「單獨一個 ? 就拒絕」的檢查，被當成 /… 執行——被產品明文拒絕的 request-target 跑起來了。",
			reason:   string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.ForceQuery = false
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed after opaque rewrite",
			contract: "adapter 把空的 Opaque 設成非空時，r.URL.RequestURI() 不再是 path?query；internal/web/locale.go 會把這個字串寫進語言切換連結的 return_to。",
			reason:   string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.Opaque = "/mutate"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed after raw path rewrite",
			contract: "adapter 改 RawPath、Path 不變時，EscapedPath() 與 RequestURI() 換成另一種 wire 編碼，return_to 跟著變；解碼後的 Path 不變，只讀 Path 的 handler 看不到差別。",
			reason:   string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.RawPath = "/%6dutate"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed after opaque is absolutised by a scheme",
			contract: "Opaque 以 // 開頭時 stdlib 會輸出 scheme + : + opaque，RequestURI() 變成一個絕對 URL 並進到 return_to；scheme 單獨改沒有讀者，它是 Opaque 絕對化時的前綴。",
			reason:   string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.Opaque = "//example.invalid/mutate"
				authed.URL.Scheme = "https"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed after request uri field rewrite",
			contract: "放行的 Request.RequestURI 必須與進 boundary 時相同。目前沒有 handler 讀這個欄位，所以這裡釘的是契約不是後果：任何日後用 url.ParseRequestURI(r.RequestURI) 重解析的人，拿到的會是 adapter 另給的一條 request-target。",
			reason:   string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.RequestURI = "/other"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed after scheme rewrite",
			contract: "伺服器端 request 的 URL.Scheme 本來就是空的，真協定在 r.TLS。目前沒有 operator 路徑讀它，這裡釘的是契約：adapter 不得改寫它。",
			reason:   string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.Scheme = "https"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed after url host rewrite",
			contract: "路由用的 Host 在 r.Host，已經有另一列守著；URL.RequestURI() 不含 URL.Host。目前沒有 operator 路徑讀它，這裡釘的是契約：adapter 不得改寫它。",
			reason:   string(operatorAuthSuccessRoutingChanged),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				authed := operatorauth.WithPrincipal(r, principal)
				authed.URL.Host = "hub.example.invalid:8787"
				return authed, operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:     "allowed with a decision naming another principal",
			contract: "adapter 的 Decision 與 request context 必須是同一個 Principal；不一致代表 adapter 的兩個輸出管道自相矛盾，整份授權結果作廢，handler 不被呼叫。目前成功路徑沒有人讀 decision.Principal，所以這裡釘的是 adapter 契約，不是「operator 以錯誤身分執行了操作」。",
			reason:   string(operatorAuthSuccessDecisionPrincipalDiffers),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				decisionPrincipal := principal
				decisionPrincipal.NodeStableID = "node-stable-2"
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized,
					Principal: decisionPrincipal,
				}
			},
		},
		{
			name:   "allowed with a source address that is not this connection's peer",
			reason: string(operatorAuthSuccessSourceNotPeer),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				principal.SourceAddr = "100.100.10.21"
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed without a grant for the required permission",
			reason: string(operatorAuthSuccessCapabilityNotGranted),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				principal.GrantedCapabilities.Admin = ""
				principal.AuthorizedCapability = ""
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed without a stable tailnet user",
			reason: string(operatorAuthSuccessIdentitySubjectEmpty),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				principal.TailnetUserID = ""
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed without a node stable ID",
			reason: string(operatorAuthSuccessIdentityNodeEmpty),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				principal.NodeStableID = ""
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed without a tailnet user login",
			reason: string(operatorAuthSuccessIdentityLoginEmpty),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				principal.TailnetUserLogin = ""
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed with non-authorized decision metadata",
			reason: string(operatorAuthSuccessDecisionNotAuthorized),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusCreated, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "allowed with a non-LocalAPI auth method",
			reason: string(operatorAuthSuccessIdentityMethodNotLocalAPI),
			authorizer: func(r *http.Request, _ operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := boundaryPrincipal(operatorauth.Admin)
				principal.AuthMethod = "future-authenticator"
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
		{
			name:   "denial labelled authorized",
			reason: operatorAuthAdapterContradictoryDetail,
			authorizer: func(*http.Request, operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				return nil, operatorauth.Decision{
					HTTPStatus: http.StatusForbidden, Code: operatorauth.Authorized,
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := boundaryStore(t)
			inner := http.NewServeMux()
			mutations := 0
			inner.HandleFunc("POST /mutate", func(http.ResponseWriter, *http.Request) { mutations++ })
			policy := map[string]operatorRoutePolicy{
				"POST /mutate": {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
			}
			boundary := newOperatorBoundary(inner, test.authorizer, st, policy, testOperatorAuthority)
			rec := httptest.NewRecorder()
			target := test.target
			if target == "" {
				target = "/mutate"
			}
			req := newBoundaryRequest(http.MethodPost, target, nil)
			req.RemoteAddr = "100.100.10.20:4321"
			boundary.ServeHTTP(rec, req)
			contract := ""
			if test.contract != "" {
				contract = "; " + test.contract
			}
			if rec.Code != http.StatusServiceUnavailable || mutations != 0 {
				t.Errorf("contradictory authorization success was served: status=%d mutations=%d body=%s; want %d with mutations=0 so an adapter result the boundary cannot verify never runs an operator action%s",
					rec.Code, mutations, rec.Body.String(), http.StatusServiceUnavailable, contract)
			}
			entries, err := st.Audit("", 10)
			if err != nil || len(entries) != 1 || entries[0].Action != store.AuditOperatorDenied ||
				entries[0].AuthDecision != string(operatorauth.AuthConfigurationInvalid) ||
				entries[0].AuthSubject != "" || entries[0].AuthNodeID != "" ||
				entries[0].AuthCapability != "" || entries[0].AuthMethod != "" {
				t.Fatalf("contradictory authorization audit=%+v err=%v%s", entries, err, contract)
			}
			if !strings.Contains(entries[0].Detail, test.reason) {
				t.Errorf("audit Detail got=%q, expected reason=%q; 稽核列是這個拒絕唯一的耐久紀錄；分不出十三種成因，oncall 就沒有下一步。",
					entries[0].Detail, test.reason)
			}
		})
	}
}

func TestAuthorizedResultWithoutAURLIsRefusedEvenWhenNothingChanged(t *testing.T) {
	principal := boundaryPrincipal(operatorauth.Admin)
	authed := operatorauth.WithPrincipal(&http.Request{}, principal)
	decision := operatorauth.Decision{
		Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
	}
	_, breach := validateAuthorizedResult(requestRouting{}, authed, decision, operatorauth.Admin)
	if breach != operatorAuthSuccessRequestIncomplete {
		t.Errorf("breach=%q, want %q; adapter 回傳的 request 必須有非 nil 的 URL。兩邊都沒有 URL 時，欄位比較會說「什麼都沒變」而放行，接著 handler 對 r.URL 解參照會 panic。fail-closed 的正確結果是拒絕，不是 panic。",
			breach, operatorAuthSuccessRequestIncomplete)
	}
}

func TestAuthorizedResultWithUnclassifiedPermissionBlamesTheRouteTable(t *testing.T) {
	request := newBoundaryRequest(http.MethodPost, "/mutate", nil)
	request.RemoteAddr = "100.100.10.20:4321"
	before := routingOf(request)
	principal := boundaryPrincipal(operatorauth.Admin)
	authed := operatorauth.WithPrincipal(request, principal)
	decision := operatorauth.Decision{
		Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
	}
	_, breach := validateAuthorizedResult(before, authed, decision, operatorauth.Admin+1)
	if breach != operatorAuthSuccessPermissionUnclassified {
		t.Errorf("breach=%q, want %q; Hub 自己的路由表多了一個沒有對應 grant 的權限等級時，稽核要說是路由表的問題，不是說這個 principal 少了一張 capability。",
			breach, operatorAuthSuccessPermissionUnclassified)
	}
}

func TestCrossOriginProtectionRunsAfterAuthAndBeforeHandler(t *testing.T) {
	st := boundaryStore(t)
	inner := http.NewServeMux()
	mutations := 0
	pathValue := ""
	inner.HandleFunc("POST /machines/{id}/channel", func(w http.ResponseWriter, r *http.Request) {
		mutations++
		pathValue = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	policy := map[string]operatorRoutePolicy{
		"POST /machines/{id}/channel": {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	}
	authorizer := &boundaryAuthorizer{allow: true}
	boundary := newOperatorBoundary(inner, authorizer, st, policy, testOperatorAuthority)

	crossSite := newBoundaryRequest(http.MethodPost, "/machines/samplehub1/channel", nil)
	crossSite.RemoteAddr = "100.100.10.20:4321"
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSite.Header.Set("User-Agent", "browser-test/1")
	rec := httptest.NewRecorder()
	boundary.ServeHTTP(rec, crossSite)
	if rec.Code != http.StatusForbidden || mutations != 0 {
		t.Fatalf("cross-site request reached handler: status=%d mutations=%d", rec.Code, mutations)
	}
	if calls := authorizer.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.Admin {
		t.Fatalf("CSRF order did not authenticate exactly once first: %v", calls)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("CSRF audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditOperatorDenied || entry.AuthDecision != string(operatorauth.Authorized) ||
		entry.BoundaryDecision != csrfDecisionCode ||
		entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
		entry.AuthCapability != "example.com/cap/clawctl-admin" ||
		entry.AuthMethod != operatorauth.AuthMethodLocalAPI || entry.SourceKind != operator.SourceKindWeb {
		t.Fatalf("CSRF denial lost verified principal: %+v", entry)
	}

	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "same-origin", "Origin": "http://100.64.200.2:8787"},
		{}, // Non-browser CLI: no Origin/Sec-Fetch-Site, but auth still ran above.
	} {
		rec = httptest.NewRecorder()
		req := newBoundaryRequest(http.MethodPost, "/machines/samplehub1/channel", nil)
		req.RemoteAddr = "100.100.10.20:4321"
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		boundary.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("allowed request status=%d body=%s headers=%v", rec.Code, rec.Body.String(), headers)
		}
	}
	if mutations != 2 || pathValue != "samplehub1" {
		t.Fatalf("allowed requests/path value lost: mutations=%d pathValue=%q", mutations, pathValue)
	}
}

func TestBrowserBoundaryDenialsLeaveDomainAndIdempotencyUntouched(t *testing.T) {
	st := boundaryStore(t)
	machineMux := http.NewServeMux()
	h := &hub{store: st}
	h.machineAndPublicRoutes(machineMux)
	machine := enrollViaHTTP(t, machineMux, st, "csrf-machine")
	if err := st.RecordObservation(machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
		OpenClaw: model.OpenClaw{Present: true},
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &boundaryAuthorizer{allow: true}
	handler, err := newHubHTTPHandler(h, ui, authorizer, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"channel":"canary","expected_revision":0,"confirm_display_name":"csrf-machine"}`
	for _, test := range []struct {
		name, host, fetchSite, origin, code string
		status                              int
	}{
		{name: "cross-site", host: testOperatorAuthority, fetchSite: "cross-site",
			status: http.StatusForbidden, code: csrfDecisionCode},
		{name: "DNS rebinding with internally same origin", host: "evil.example:8787",
			fetchSite: "same-origin", origin: "http://evil.example:8787",
			status: http.StatusMisdirectedRequest, code: operatorAuthorityDecisionCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := newBoundaryRequest(http.MethodPut, "/v1/operator/machines/"+machine.id+"/channel", strings.NewReader(body))
			req.RemoteAddr = "100.100.10.20:4321"
			req.Host = test.host
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "browser-denial-"+strings.ReplaceAll(test.name, " ", "-"))
			req.Header.Set("Sec-Fetch-Site", test.fetchSite)
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("denied PUT status=%d, want %d body=%s", rec.Code, test.status, rec.Body.String())
			}
			var apiError model.APIError
			if err := json.Unmarshal(rec.Body.Bytes(), &apiError); err != nil || apiError.Code != test.code {
				t.Fatalf("JSON contract=%+v err=%v body=%s", apiError, err, rec.Body.String())
			}
			machineAfter, err := st.GetMachine(machine.id)
			if err != nil || machineAfter.Channel != "" || machineAfter.ChannelRevision != 0 {
				t.Fatalf("boundary denial changed machine=%+v err=%v", machineAfter, err)
			}
			var ledgerRows int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledgerRows); err != nil || ledgerRows != 0 {
				t.Fatalf("boundary denial occupied idempotency ledger: rows=%d err=%v", ledgerRows, err)
			}
		})
	}
	if calls := authorizer.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.Admin {
		t.Fatalf("DNS rebinding must fail before auth; calls=%v", calls)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("boundary denials should write exactly two rows: entries=%+v err=%v", entries, err)
	}
	decisions := map[string]bool{}
	for _, entry := range entries {
		if entry.Action != store.AuditOperatorDenied || entry.MachineID != machine.id || entry.Subject != "csrf-machine" {
			t.Fatalf("boundary denial leaked a domain audit row: %+v", entry)
		}
		if entry.BoundaryDecision != "" {
			decisions[entry.BoundaryDecision] = true
		} else {
			decisions[entry.AuthDecision] = true
		}
	}
	if !decisions[csrfDecisionCode] || !decisions[operatorAuthorityDecisionCode] {
		t.Fatalf("boundary audit decisions=%v", decisions)
	}
}

func TestMachineLifecycleBoundaryDenialsLeaveLifecycleAndIdempotencyUntouched(t *testing.T) {
	st := boundaryStore(t)
	machineMux := http.NewServeMux()
	h := &hub{store: st}
	h.machineAndPublicRoutes(machineMux)
	machine := enrollViaHTTP(t, machineMux, st, "boundary-lifecycle")
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &boundaryAuthorizer{allow: true}
	handler, err := newHubHTTPHandler(h, ui, authorizer, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"desired_state":"retired","expected_revision":0,"confirm_display_name":"boundary-lifecycle","preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"must not reach domain"}`
	for _, test := range []struct {
		name, host, fetchSite, origin, code string
		status                              int
	}{
		{name: "cross-origin", host: testOperatorAuthority, fetchSite: "cross-site",
			origin: "http://evil.example:8787", status: http.StatusForbidden, code: csrfDecisionCode},
		{name: "wrong Host", host: "evil.example:8787", fetchSite: "same-origin",
			origin: "http://evil.example:8787", status: http.StatusMisdirectedRequest,
			code: operatorAuthorityDecisionCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := newBoundaryRequest(http.MethodPut,
				"/v1/operator/machines/"+machine.id+"/lifecycle", strings.NewReader(body))
			req.Host = test.host
			req.RemoteAddr = "100.100.10.20:4321"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "denied-lifecycle-"+strings.ReplaceAll(test.name, " ", "-"))
			req.Header.Set("Sec-Fetch-Site", test.fetchSite)
			req.Header.Set("Origin", test.origin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("denied lifecycle PUT=%d, want %d body=%s", rec.Code, test.status, rec.Body.String())
			}
			var apiError model.APIError
			if err := json.Unmarshal(rec.Body.Bytes(), &apiError); err != nil || apiError.Code != test.code {
				t.Fatalf("denial JSON=%+v err=%v body=%s", apiError, err, rec.Body.String())
			}
			after, err := st.GetMachine(machine.id)
			if err != nil || after.RetiredAt != nil {
				t.Fatalf("boundary denial changed lifecycle: machine=%+v err=%v", after, err)
			}
			var ledgerRows int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledgerRows); err != nil || ledgerRows != 0 {
				t.Fatalf("boundary denial occupied idempotency ledger: rows=%d err=%v", ledgerRows, err)
			}
		})
	}
	if calls := authorizer.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.Admin {
		t.Fatalf("cross-origin must authenticate as admin; wrong Host must fail before auth: calls=%v", calls)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("lifecycle boundary denials audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditOperatorDenied || entry.MachineID != machine.id ||
			entry.Subject != "boundary-lifecycle" || !strings.Contains(entry.Detail, "required=admin") {
			t.Fatalf("boundary denial reached lifecycle domain or lost target evidence: %+v", entry)
		}
	}
}

func TestOperatorEnrollmentCreateBoundaryDenialsLeaveIssuanceUntouched(t *testing.T) {
	st := boundaryStore(t)
	h := &hub{store: st}
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &boundaryAuthorizer{allow: true}
	handler, err := newHubHTTPHandler(h, ui, authorizer, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"display_name":"boundary-host","ttl_seconds":3600,"preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"must never run"}`
	for _, test := range []struct {
		name, host, fetchSite, origin, code string
		status                              int
	}{
		{name: "cross-origin", host: testOperatorAuthority, fetchSite: "cross-site",
			origin: "http://evil.example:8787", status: http.StatusForbidden, code: csrfDecisionCode},
		{name: "DNS rebinding wrong Host", host: "evil.example:8787", fetchSite: "same-origin",
			origin: "http://evil.example:8787", status: http.StatusMisdirectedRequest,
			code: operatorAuthorityDecisionCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := newBoundaryRequest(http.MethodPost, "/v1/operator/enrollment-tokens", strings.NewReader(body))
			req.Host = test.host
			req.RemoteAddr = "100.100.10.20:4321"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "denied-enroll-"+strings.ReplaceAll(test.name, " ", "-"))
			req.Header.Set("Sec-Fetch-Site", test.fetchSite)
			req.Header.Set("Origin", test.origin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("denied enrollment status=%d, want %d body=%s", rec.Code, test.status, rec.Body.String())
			}
			var apiError model.APIError
			if err := json.Unmarshal(rec.Body.Bytes(), &apiError); err != nil || apiError.Code != test.code {
				t.Fatalf("denial JSON=%+v err=%v body=%s", apiError, err, rec.Body.String())
			}
			for table, want := range map[string]int{
				"machine_registry":     0,
				"enrollment_tokens":    0,
				"operator_idempotency": 0,
			} {
				var rows int
				if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&rows); err != nil || rows != want {
					t.Fatalf("boundary denial changed %s rows=%d want=%d err=%v", table, rows, want, err)
				}
			}
		})
	}
	if calls := authorizer.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.Admin {
		t.Fatalf("cross-origin should authenticate once; wrong Host must fail before auth: calls=%v", calls)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("enrollment boundary denials audit=%+v err=%v", entries, err)
	}
	decisions := map[string]bool{}
	for _, entry := range entries {
		if entry.Action != store.AuditOperatorDenied || entry.Action == store.AuditEnrollToken {
			t.Fatalf("boundary denial reached enrollment domain audit: %+v", entry)
		}
		if entry.BoundaryDecision != "" {
			decisions[entry.BoundaryDecision] = true
		} else {
			decisions[entry.AuthDecision] = true
		}
	}
	if !decisions[csrfDecisionCode] || !decisions[operatorAuthorityDecisionCode] {
		t.Fatalf("enrollment boundary decisions=%v", decisions)
	}
}

func TestPendingEnrollmentTokenRevocationBoundaryDenialsLeaveTicketRegistryAndIdempotencyUntouched(t *testing.T) {
	st := boundaryStore(t)
	machineID, token, err := st.CreateEnrollTokenFor("boundary-pending-revoke", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil {
		t.Fatal(err)
	}
	h := &hub{store: st}
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &boundaryAuthorizer{allow: true}
	handler, err := newHubHTTPHandler(h, ui, authorizer, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}

	apiPath := "/v1/operator/machines/" + machineID + "/enrollment-token/revocations"
	webPath := "/machines/" + machineID + "/revoke-token"
	for _, test := range []struct {
		name, path, host, fetchSite, origin, code string
		status                                    int
		representation                            operatorRepresentation
		sourceKind                                string
	}{
		{name: "JSON cross-origin", path: apiPath, host: testOperatorAuthority,
			fetchSite: "cross-site", origin: "http://evil.example:8787",
			status: http.StatusForbidden, code: csrfDecisionCode,
			representation: operatorJSON, sourceKind: operator.SourceKindOperatorAPI},
		{name: "JSON wrong Host", path: apiPath, host: "evil.example:8787",
			fetchSite: "same-origin", origin: "http://evil.example:8787",
			status: http.StatusMisdirectedRequest, code: operatorAuthorityDecisionCode,
			representation: operatorJSON, sourceKind: operator.SourceKindOperatorAPI},
		{name: "Web cross-origin", path: webPath, host: testOperatorAuthority,
			fetchSite: "cross-site", origin: "http://evil.example:8787",
			status: http.StatusForbidden, code: csrfDecisionCode,
			representation: operatorHTML, sourceKind: operator.SourceKindWeb},
		{name: "Web wrong Host", path: webPath, host: "evil.example:8787",
			fetchSite: "same-origin", origin: "http://evil.example:8787",
			status: http.StatusMisdirectedRequest, code: operatorAuthorityDecisionCode,
			representation: operatorHTML, sourceKind: operator.SourceKindWeb},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := "denied-pending-revoke-" + strings.ToLower(strings.ReplaceAll(test.name, " ", "-"))
			body := `{"preview_digest":"` + preview.PreviewDigest + `","reason":"boundary denial"}`
			contentType := "application/json"
			if test.representation == operatorHTML {
				body = "preview_digest=" + preview.PreviewDigest + "&reason=boundary-denial&idempotency_key=" + key
				contentType = "application/x-www-form-urlencoded"
			}
			req := newBoundaryRequest(http.MethodPost, test.path, strings.NewReader(body))
			req.Host = test.host
			req.RemoteAddr = "100.100.10.20:4321"
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("Sec-Fetch-Site", test.fetchSite)
			req.Header.Set("Origin", test.origin)
			req.Header.Set("User-Agent", "browser-boundary-test/1")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("denied revocation status=%d, want %d body=%s", rec.Code, test.status, rec.Body.String())
			}
			contentType = rec.Header().Get("Content-Type")
			switch test.representation {
			case operatorJSON:
				if !strings.HasPrefix(contentType, "application/json") {
					t.Fatalf("JSON denial content-type=%q body=%s", contentType, rec.Body.String())
				}
				var apiError model.APIError
				if err := json.Unmarshal(rec.Body.Bytes(), &apiError); err != nil ||
					apiError.Code != test.code || apiError.Message == "" {
					t.Fatalf("JSON denial=%+v err=%v body=%s", apiError, err, rec.Body.String())
				}
			case operatorHTML:
				if !strings.HasPrefix(contentType, "text/html") || json.Valid(rec.Body.Bytes()) ||
					!strings.Contains(rec.Body.String(), "<!doctype html>") ||
					!strings.Contains(rec.Body.String(), "<code>"+test.code+"</code>") {
					t.Fatalf("HTML denial content-type=%q body=%s", contentType, rec.Body.String())
				}
			default:
				t.Fatalf("unsupported test representation %d", test.representation)
			}
			if rec.Header().Get("Cache-Control") != "no-store" ||
				rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("denial security headers=%v", rec.Header())
			}
			if strings.Contains(rec.Body.String(), token) {
				t.Fatal("boundary denial leaked the pending enrollment token")
			}

			after, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
			if err != nil || after.PreviewDigest != preview.PreviewDigest ||
				after.MachineID != preview.MachineID || after.DisplayName != preview.DisplayName ||
				!after.TokenCreatedAt.Equal(preview.TokenCreatedAt) ||
				!after.TokenExpiresAt.Equal(preview.TokenExpiresAt) {
				t.Fatalf("boundary denial changed exact pending ticket: before=%+v after=%+v err=%v",
					preview, after, err)
			}
			var registryRows, expected, retired, pendingRows, ledgerRows int
			var displayName string
			if err := st.DB().QueryRow(`SELECT COUNT(*),COALESCE(MAX(display_name),''),
			 COALESCE(MAX(expected),0),COALESCE(MAX(CASE WHEN retired_at IS NOT NULL THEN 1 ELSE 0 END),0)
			 FROM machine_registry WHERE machine_id=?`, machineID).Scan(
				&registryRows, &displayName, &expected, &retired); err != nil {
				t.Fatal(err)
			}
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens
			 WHERE used_by=? AND used_at IS NULL`, machineID).Scan(&pendingRows); err != nil {
				t.Fatal(err)
			}
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledgerRows); err != nil {
				t.Fatal(err)
			}
			if registryRows != 1 || displayName != "boundary-pending-revoke" || expected != 1 ||
				retired != 0 || pendingRows != 1 || ledgerRows != 0 {
				t.Fatalf("boundary denial changed state: registry=%d name=%q expected=%d retired=%d pending=%d ledger=%d",
					registryRows, displayName, expected, retired, pendingRows, ledgerRows)
			}
			latest, err := st.Audit("", 1)
			if err != nil || len(latest) != 1 || latest[0].Action != store.AuditOperatorDenied ||
				latest[0].MachineID != machineID || latest[0].Subject != "boundary-pending-revoke" ||
				latest[0].BoundaryDecision != test.code || latest[0].SourceKind != test.sourceKind ||
				!strings.Contains(latest[0].Detail, "required=admin") {
				t.Fatalf("boundary denial audit=%+v err=%v", latest, err)
			}
		})
	}

	if calls := authorizer.snapshotCalls(); len(calls) != 2 ||
		calls[0] != operatorauth.Admin || calls[1] != operatorauth.Admin {
		t.Fatalf("cross-origin requests must authenticate; wrong Host requests must not: calls=%v", calls)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 4 {
		t.Fatalf("revocation boundary denials audit=%+v err=%v", entries, err)
	}
	decisions := map[string]int{}
	sourceKinds := map[string]int{}
	for _, entry := range entries {
		if entry.Action != store.AuditOperatorDenied ||
			entry.MachineID != machineID || entry.Subject != "boundary-pending-revoke" ||
			!strings.Contains(entry.Detail, "required=admin") {
			t.Fatalf("boundary denial reached revocation or lost target evidence: %+v", entry)
		}
		decisions[entry.BoundaryDecision]++
		sourceKinds[entry.SourceKind]++
	}
	if decisions[csrfDecisionCode] != 2 || decisions[operatorAuthorityDecisionCode] != 2 ||
		sourceKinds[operator.SourceKindOperatorAPI] != 2 || sourceKinds[operator.SourceKindWeb] != 2 {
		t.Fatalf("revocation boundary audit decisions=%v source_kinds=%v", decisions, sourceKinds)
	}
}

func TestOperatorBoundarySecurityHeadersAndDestinationParsing(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	boundary := newOperatorBoundary(inner, &boundaryAuthorizer{allow: true}, nil,
		map[string]operatorRoutePolicy{"GET /{$}": {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}},
		testOperatorAuthority)
	rec := httptest.NewRecorder()
	req := newBoundaryRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	boundary.ServeHTTP(rec, req)
	for _, name := range []string{"Cache-Control", "Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if rec.Header().Get(name) == "" {
			t.Errorf("missing security header %s", name)
		}
	}
	if got, want := rec.Header().Get("Content-Security-Policy"), "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"; got != want {
		t.Fatalf("locked CSP=%q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
	missing := httptest.NewRecorder()
	missingReq := newBoundaryRequest(http.MethodGet, "/no-such-operator-page", nil)
	missingReq.RemoteAddr = "100.100.10.20:4321"
	boundary.ServeHTTP(missing, missingReq)
	if got := missing.Header().Get("Content-Security-Policy"); got != lockedOperatorContentSecurityPolicy {
		t.Fatalf("unmatched route CSP=%q", got)
	}
	for _, test := range []struct {
		listen string
		want   netip.Addr
		ok     bool
	}{
		{listen: "100.64.200.2:8787", want: netip.MustParseAddr("100.64.200.2"), ok: true},
		{listen: "[fd7a:115c:a1e0::1234]:8787", want: netip.MustParseAddr("fd7a:115c:a1e0::1234"), ok: true},
		{listen: "hub.example:8787"},
		{listen: "100.64.200.2"},
		{listen: "100.100.100.100:8787"},
		{listen: "[fd7a:115c:a1e0:b1a::1]:8787"},
	} {
		got, err := operatorDestinationFromListen(test.listen)
		if (err == nil) != test.ok || (test.ok && got != test.want) {
			t.Errorf("operatorDestinationFromListen(%q)=(%v,%v), want (%v, ok=%t)", test.listen, got, err, test.want, test.ok)
		}
	}
}

func terminalDocumentCSPForTest(useTLS bool) string {
	scheme := "ws"
	if useTLS {
		scheme = "wss"
	}
	return "default-src 'none'; script-src '" + terminalassets.XTermJSHash + "' '" +
		terminalassets.FitJSHash + "' '" + terminalassets.PageJSHash +
		"'; connect-src " + scheme + "://" + testOperatorAuthority +
		"; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
}

func TestOmittedSecurityProfileFailsClosedWithLockedCSP(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /future", func(http.ResponseWriter, *http.Request) {
		t.Fatal("omitted security profile reached the handler")
	})
	policies := map[string]operatorRoutePolicy{
		"GET /future": {Permission: operatorauth.View, Representation: operatorHTML, SourceKind: operator.SourceKindWeb},
	}
	if err := validateOperatorRoutePolicies([]string{"GET /future"}, policies); err == nil {
		t.Fatal("omitted security profile was accepted")
	}
	boundary := newOperatorBoundary(inner, &boundaryAuthorizer{allow: true}, boundaryStore(t), policies, testOperatorAuthority)
	rec := httptest.NewRecorder()
	req := newBoundaryRequest(http.MethodGet, "/future", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	boundary.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != lockedOperatorContentSecurityPolicy || strings.Contains(got, "script-src") {
		t.Fatalf("omitted profile CSP=%q", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", rec.Header().Get("Cache-Control"))
	}
}

func TestCrossOriginDenialCannotUseTerminalSecurityProfile(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("POST /mutate", func(http.ResponseWriter, *http.Request) {
		t.Fatal("cross-origin mutation reached the handler")
	})
	inner.HandleFunc(operatorTerminalDocumentPattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	policies := map[string]operatorRoutePolicy{
		"POST /mutate":                  {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
		operatorTerminalDocumentPattern: {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityTerminal},
	}
	boundary := newOperatorBoundary(inner, &boundaryAuthorizer{allow: true}, boundaryStore(t), policies, testOperatorAuthority)
	req := newBoundaryRequest(http.MethodPost, "/mutate", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "http://evil.example:8787")
	rec := httptest.NewRecorder()
	boundary.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Header().Get("Content-Security-Policy")
	if got != lockedOperatorContentSecurityPolicy || strings.Contains(got, "script-src") || strings.Contains(got, terminalassets.PageJSHash) {
		t.Fatalf("CSRF denial CSP=%q", got)
	}

	tlsReq := newBoundaryRequest(http.MethodGet, "/machines/machine-1/terminals/session-1", nil)
	tlsReq.RemoteAddr = "100.100.10.20:4321"
	tlsReq.TLS = &tls.ConnectionState{}
	tlsRec := httptest.NewRecorder()
	boundary.ServeHTTP(tlsRec, tlsReq)
	if got := tlsRec.Header().Get("Content-Security-Policy"); got != terminalDocumentCSPForTest(true) {
		t.Fatalf("tls terminal CSP=%q", got)
	}
}

func TestRenderedTerminalDocumentUsesTerminalSecurityProfile(t *testing.T) {
	st := boundaryStore(t)
	now := time.Now().UTC()
	token, err := st.CreateEnrollToken("samplehub1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "samplehub1", OS: "linux", Arch: "amd64", UnixUser: "example-user",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		"42", "operator@example.com", machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenAgentSession(store.OpenAgentSessionRequest{
		SessionID: "session-1", MachineID: machineID,
		OperatorTailnetUserID: "42", OperatorTailnetUserLogin: "operator@example.com",
		IdempotencyKey: "open-session-1", RequestDigest: "sha256:session-1",
		Audit: store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHubHTTPHandler(&hub{store: st}, ui, &boundaryAuthorizer{allow: true}, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := newBoundaryRequest(http.MethodGet, "/machines/"+machineID+"/terminals/session-1", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "</html>") {
		t.Fatalf("status=%d body tail=%s", rec.Code, tailOf(rec.Body.String()))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", rec.Header().Get("Cache-Control"))
	}
	if got, want := rec.Header().Get("Content-Security-Policy"), terminalDocumentCSPForTest(false); got != want {
		t.Fatalf("terminal CSP=%q\nwant %q", got, want)
	}
	scripts := scriptContents(rec.Body.String())
	wantScripts := [][]byte{terminalassets.XTermJS, terminalassets.FitJS, terminalassets.PageJS}
	wantHashes := []string{terminalassets.XTermJSHash, terminalassets.FitJSHash, terminalassets.PageJSHash}
	if len(scripts) != len(wantScripts) {
		t.Fatalf("script count=%d", len(scripts))
	}
	for i, script := range scripts {
		sum := sha256.Sum256([]byte(script))
		got := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
		if got != wantHashes[i] || script != string(wantScripts[i]) {
			t.Fatalf("script %d hash=%s want %s emitted=%d embedded=%d", i, got, wantHashes[i], len(script), len(wantScripts[i]))
		}
	}
	dashboard := httptest.NewRecorder()
	dashReq := newBoundaryRequest(http.MethodGet, "/", nil)
	dashReq.RemoteAddr = "100.100.10.20:4321"
	handler.ServeHTTP(dashboard, dashReq)
	if got := dashboard.Header().Get("Content-Security-Policy"); got != lockedOperatorContentSecurityPolicy {
		t.Fatalf("dashboard CSP=%q", got)
	}
}

// connect-src 的 host 是呼叫端傳入的 authority，也就是啟動時釘住的 listener。
// writeSecurityHeaders 比對 Host 之前就寫 CSP，所以 Host 不符的 421 仍帶著
// 這個標頭。表格直接呼叫函式。最後一列走 ServeHTTP，呼叫點必須傳 boundary
// 的 authority，而不是請求的 Host。
func TestContentSecurityPolicyNamesPinnedListenerNotRequestHost(t *testing.T) {
	const authority = "100.64.0.1:8443"
	const hostileHost = "evil.example:80"
	hostile := httptest.NewRequest(http.MethodGet, "http://"+hostileHost+"/machines/machine-1/terminals/session-1", nil)
	hostile.Host = hostileHost
	withTLS := hostile.Clone(hostile.Context())
	withTLS.TLS = &tls.ConnectionState{}

	scriptSrcBody := func(policy string) string {
		const marker = "script-src "
		start := strings.Index(policy, marker)
		if start < 0 {
			return ""
		}
		rest := policy[start+len(marker):]
		end := strings.IndexByte(rest, ';')
		if end < 0 {
			return ""
		}
		return rest[:end]
	}
	wantScriptSrc := "'" + terminalassets.XTermJSHash + "' '" +
		terminalassets.FitJSHash + "' '" + terminalassets.PageJSHash + "'"

	tests := []struct {
		name       string
		profile    operatorSecurityProfile
		req        *http.Request
		connectSrc string
		exact      string
	}{
		{
			name:       "terminal http",
			profile:    operatorSecurityTerminal,
			req:        hostile,
			connectSrc: "connect-src ws://" + authority + ";",
		},
		{
			name:       "terminal tls",
			profile:    operatorSecurityTerminal,
			req:        withTLS,
			connectSrc: "connect-src wss://" + authority + ";",
		},
		{
			name:       "terminal nil request",
			profile:    operatorSecurityTerminal,
			req:        nil,
			connectSrc: "connect-src ws://" + authority + ";",
		},
		{
			name:    "locked",
			profile: operatorSecurityLocked,
			req:     hostile,
			exact:   lockedOperatorContentSecurityPolicy,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := contentSecurityPolicy(test.profile, authority, test.req)
			if test.exact != "" {
				if got != test.exact {
					t.Fatalf("CSP=%q, want %q", got, test.exact)
				}
				if strings.Contains(got, "evil.example") || strings.Contains(got, authority) {
					t.Fatalf("locked CSP names a host: %q", got)
				}
				return
			}
			if !strings.Contains(got, test.connectSrc) {
				t.Fatalf("CSP=%q, missing %q", got, test.connectSrc)
			}
			if strings.Contains(got, "evil.example") {
				t.Fatalf("CSP names the request Host: %q", got)
			}
			if strings.Count(got, "script-src") != 1 || scriptSrcBody(got) != wantScriptSrc {
				t.Fatalf("script-src=%q, want %q", scriptSrcBody(got), wantScriptSrc)
			}
		})
	}

	t.Run("writeSecurityHeaders on misdirected request", func(t *testing.T) {
		inner := http.NewServeMux()
		inner.HandleFunc(operatorTerminalDocumentPattern, func(http.ResponseWriter, *http.Request) {
			t.Fatal("hostile Host reached the terminal document")
		})
		boundary := newOperatorBoundary(inner, &boundaryAuthorizer{allow: true}, nil,
			map[string]operatorRoutePolicy{
				operatorTerminalDocumentPattern: {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityTerminal},
			}, authority)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://"+hostileHost+"/machines/machine-1/terminals/session-1", nil)
		req.Host = hostileHost
		req.RemoteAddr = "100.100.10.20:4321"
		boundary.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		got := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(got, "connect-src ws://"+authority+";") || strings.Contains(got, "evil.example") {
			t.Fatalf("CSP=%q, missing %q", got, "connect-src ws://"+authority+";")
		}
		if strings.Count(got, "script-src") != 1 || scriptSrcBody(got) != wantScriptSrc {
			t.Fatalf("script-src=%q, want %q", scriptSrcBody(got), wantScriptSrc)
		}
	})
}

func scriptContents(html string) []string {
	var out []string
	rest := html
	for {
		start := strings.Index(rest, "<script>")
		if start < 0 {
			return out
		}
		rest = rest[start+len("<script>"):]
		end := strings.Index(rest, "</script>")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end+len("</script>"):]
	}
}

func tailOf(body string) string {
	if len(body) <= 400 {
		return body
	}
	return body[len(body)-400:]
}

// Publishing or assigning a settings policy changes how every targeted machine
// behaves, which puts it in the same class as channel assignment, machine
// lifecycle and tailnet exceptions: admin, on every plane that offers it.
//
// ⚠ 這條規則是被違反過才寫下來的。5afb64b 把 JSON 那四條開成 operate，
// 729db66 把 Web 那四條開成 admin，兩邊各自有 per-route 測試，兩邊都過 ——
// 因為那些測試抄的是程式碼，不是規則。一個 operate 憑證於是能繞過主控台
// 要求 admin 的那道門。這裡掃整份 manifest，不維護第二份名單。
func TestSettingWritesCostAdminOnEveryPlane(t *testing.T) {
	writes := 0
	for pattern, policy := range operatorRoutePolicies {
		method, path, _ := strings.Cut(pattern, " ")
		if method == "GET" || method == "HEAD" {
			continue
		}
		if !strings.Contains(path, "/setting-") && !strings.Contains(path, "/machines/configuration/") {
			continue
		}
		writes++
		if policy.Permission != operatorauth.Admin {
			t.Errorf("%s 需要 %s；設定寫入在每個平面都必須是 admin", pattern, policy.Permission)
		}
	}
	// 兩個平面各四條。少一條就是有一條寫入路徑改了名字卻沒被這條規則涵蓋。
	if writes != 8 {
		t.Fatalf("掃到 %d 條設定寫入路徑，預期 8 條（JSON 4、Web 4）", writes)
	}
}

// 發佈或指派合規性原則會改變整批機器的判決，跟設定寫入同一級：每個平面都要
// admin。規則跟著工作流程走，不是跟著單一 route 走，所以這裡也掃整份 manifest。
func TestComplianceWritesCostAdminOnEveryPlane(t *testing.T) {
	writes := 0
	for pattern, policy := range operatorRoutePolicies {
		method, path, _ := strings.Cut(pattern, " ")
		if method == "GET" || method == "HEAD" {
			continue
		}
		if !strings.Contains(path, "/compliance-") && !strings.Contains(path, "/machines/compliance/") {
			continue
		}
		writes++
		if policy.Permission != operatorauth.Admin {
			t.Errorf("%s 需要 %s；合規性寫入在每個平面都必須是 admin", pattern, policy.Permission)
		}
	}
	if writes != 8 {
		t.Fatalf("掃到 %d 條合規性寫入路徑，預期 8 條（JSON 4、Web 4）", writes)
	}
}

// 發佈 catalog package、machine profile，以及把 profile 指派到機器，會讓軟體
// 安裝到機隊機器上，跟設定寫入同一級：每個平面都要 admin。
func TestCatalogWritesCostAdminOnEveryPlane(t *testing.T) {
	writes := 0
	for pattern, policy := range operatorRoutePolicies {
		method, path, _ := strings.Cut(pattern, " ")
		if method == "GET" || method == "HEAD" {
			continue
		}
		if !strings.Contains(path, "/catalog-manifests") &&
			!strings.Contains(path, "/machine-profiles") &&
			!strings.Contains(path, "/profile-assignment") &&
			!strings.Contains(path, "/apps/store/packages") &&
			!strings.Contains(path, "/apps/profiles") {
			continue
		}
		writes++
		if policy.Permission != operatorauth.Admin {
			t.Errorf("%s 實際需要 %s，預期 admin；否則非 admin operator 能把軟體安裝到機隊機器", pattern, policy.Permission)
		}
	}
	if writes != 12 {
		t.Fatalf("實際掃到 %d 條 catalog 寫入路徑，預期 12 條（JSON 6、Web 6）；少一條表示有路徑改名卻未被規則涵蓋，多一條表示有新的 catalog 寫入路徑進入", writes)
	}
}

// 開票、撤未兌換票、設定註冊上限都會決定誰能把機器加進機隊，跟設定寫入同一級：
// 每個平面都要 admin。Web 與 JSON 兩個平面對撤票用了不同的路徑名稱，所以規則
// 必須同時認得兩種名字。
func TestEnrollmentWritesCostAdminOnEveryPlane(t *testing.T) {
	writes := 0
	for pattern, policy := range operatorRoutePolicies {
		method, path, _ := strings.Cut(pattern, " ")
		if method == "GET" || method == "HEAD" {
			continue
		}
		if !strings.Contains(path, "/enrollment") && !strings.Contains(path, "/revoke-token") {
			continue
		}
		writes++
		if policy.Permission != operatorauth.Admin {
			t.Errorf("%s 實際需要 %s，預期 admin；否則非 admin operator 能決定誰可把機器加進機隊", pattern, policy.Permission)
		}
	}
	if writes != 12 {
		t.Fatalf("實際掃到 %d 條 enrollment 寫入路徑，預期 12 條（JSON 6、Web 6）；少一條表示有 enrollment 寫入路徑改名卻未被規則涵蓋（Web 撤票就是這種命名不對稱的例子），非 admin operator 可能繞過限制；多一條表示有新的 enrollment 寫入路徑進入，必須確認 operator 權限", writes)
	}
}

// 這條規則跟上面四支不同：它釘的是 exact 分級，不是 admin 地板。
// 開新計畫與撕掉計畫是 admin；執行已核准計畫、重開失敗 target，
// 以及單獨標示的 skip failed batch 是 operate。
// 兩個平面對同一動作用了不同路徑名稱，所以分級必須同時認得兩種名字。
func TestDeploymentWritesCostTheirExactCapabilityOnEveryPlane(t *testing.T) {
	operateWrites := 0
	adminWrites := 0
	for pattern, policy := range operatorRoutePolicies {
		method, path, _ := strings.Cut(pattern, " ")
		if method == "GET" || method == "HEAD" {
			continue
		}
		if !strings.Contains(path, "/deployment") {
			continue
		}
		want := operatorauth.Admin
		if strings.Contains(path, "/continu") || strings.Contains(path, "/retr") || strings.Contains(path, "/skip-failed-batch") {
			want = operatorauth.Operate
			operateWrites++
		} else {
			adminWrites++
		}
		if policy.Permission != want {
			t.Errorf("%s 實際需要 %s，預期 %s；降成 operate 會讓只有 operate 的憑證拿到高風險寫入，誤升成 admin 會讓值班無法在同一個主控台續跑已核准計畫", pattern, policy.Permission, want)
		}
	}
	if operateWrites != 12 {
		t.Fatalf("實際掃到 %d 條 deployment operate 寫入路徑，預期 12 條（continue/retry 8 加上 skip failed batch 4）；少一條表示有路徑改名沒被涵蓋，多一條表示有新的 deployment 寫入路徑進來", operateWrites)
	}
	if adminWrites != 8 {
		t.Fatalf("實際掃到 %d 條 deployment admin 寫入路徑，預期 8 條；少一條表示有路徑改名沒被涵蓋，多一條表示有新的 deployment 寫入路徑進來", adminWrites)
	}
}

func TestNavigationLanguageIsTheOnlyUnsafeViewRoute(t *testing.T) {
	registered := make([]string, 0, len(operatorRoutePolicies))
	for pattern := range operatorRoutePolicies {
		registered = append(registered, pattern)
	}
	if err := validateOperatorRoutePolicies(registered, operatorRoutePolicies); err != nil {
		t.Fatal(err)
	}
	want := operatorRoutePolicy{operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}
	if got, ok := operatorRoutePolicies["POST /preferences/navigation-language"]; !ok || got != want {
		t.Fatalf("navigation language policy=%+v ok=%t, want %+v", got, ok, want)
	}
	if _, ok := operatorRoutePolicies["GET /preferences/navigation-language/{locale}"]; ok {
		t.Fatal("the GET navigation language route is still classified")
	}

	refusedAsView := func(registered []string, policies map[string]operatorRoutePolicy) bool {
		err := validateOperatorRoutePolicies(registered, policies)
		return err != nil && strings.Contains(err.Error(), "must not be classified as view")
	}
	unsafeRoutes := 0
	for _, pattern := range registered {
		method, _, _ := strings.Cut(pattern, " ")
		if isSafeMethod(method) || pattern == navigationLanguagePattern {
			continue
		}
		unsafeRoutes++
		policies := maps.Clone(operatorRoutePolicies)
		policy := policies[pattern]
		policy.Permission = operatorauth.View
		policies[pattern] = policy
		if !refusedAsView(registered, policies) {
			t.Errorf("unsafe route %q classified view was accepted", pattern)
		}
	}
	if unsafeRoutes == 0 {
		t.Fatal("the real table has no unsafe routes to reclassify")
	}
	for _, pattern := range []string{
		"POST /preferences/other",
		"PUT /preferences/navigation-language",
		"POST /preferences/navigation-language/{locale}",
	} {
		policies := maps.Clone(operatorRoutePolicies)
		policies[pattern] = want
		if !refusedAsView(append(slices.Clone(registered), pattern), policies) {
			t.Errorf("unsafe route %q classified view was accepted", pattern)
		}
	}
}

func TestNavigationLanguageRefusesCrossSitePostAndAcceptsSameOriginView(t *testing.T) {
	st := boundaryStore(t)
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	var calls []operatorauth.Permission
	viewOnly := boundaryAuthorizeFunc(func(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision) {
		calls = append(calls, permission)
		if permission != operatorauth.View {
			return nil, operatorauth.Decision{
				HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired, Detail: "view only",
			}
		}
		principal := boundaryPrincipal(operatorauth.View)
		principal.GrantedCapabilities = operatorauth.CapabilityNames{View: principal.AuthorizedCapability}
		return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
			Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized,
			Principal: principal, Detail: "authorized",
		}
	})
	handler, err := newHubHTTPHandler(&hub{store: st}, ui, viewOnly, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	post := func(headers map[string]string) *httptest.ResponseRecorder {
		req := newBoundaryRequest(http.MethodPost, "/preferences/navigation-language",
			strings.NewReader("locale=en&return_to=%2Fmachines"))
		req.RemoteAddr = "100.100.10.20:4321"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	crossSite := post(map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"})
	if crossSite.Code != http.StatusForbidden || !strings.Contains(crossSite.Body.String(), csrfDecisionCode) ||
		crossSite.Header().Get("Set-Cookie") != "" || crossSite.Header().Get("Location") != "" {
		t.Fatalf("cross-site switch status=%d headers=%v body=%s", crossSite.Code, crossSite.Header(), crossSite.Body.String())
	}
	if len(calls) != 1 || calls[0] != operatorauth.View {
		t.Fatalf("cross-site auth calls=%v, want one view check", calls)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cross-site audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditOperatorDenied || entry.OK || entry.BoundaryDecision != csrfDecisionCode ||
		entry.AuthCapability != "example.com/cap/clawctl-view" || entry.SourceKind != operator.SourceKindWeb ||
		!strings.Contains(entry.Detail, "；required=view；pattern="+navigationLanguagePattern+"；") {
		t.Fatalf("cross-site denial audit=%+v", entry)
	}

	sameOrigin := post(map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testOperatorAuthority})
	cookies := sameOrigin.Result().Cookies()
	if sameOrigin.Code != http.StatusSeeOther || sameOrigin.Header().Get("Location") != "/machines" ||
		len(cookies) != 1 || cookies[0].Name != "clawctl_navigation_locale" || cookies[0].Value != "en" {
		t.Fatalf("same-origin switch status=%d headers=%v body=%s", sameOrigin.Code, sameOrigin.Header(), sameOrigin.Body.String())
	}
	if len(calls) != 2 || calls[1] != operatorauth.View {
		t.Fatalf("same-origin auth calls=%v, want a second view check", calls)
	}
	if entries, err := st.Audit("", 10); err != nil || len(entries) != 1 {
		t.Fatalf("same-origin switch changed the audit log: %+v err=%v", entries, err)
	}
}
