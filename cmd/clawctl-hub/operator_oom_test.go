package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

func TestOperatorOOMKills(t *testing.T) {
	f := newJobsFixture(t, "example-machine")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	now := time.Now().UTC()
	b := model.ObservationBatch{MeasuredAt: now, OOMKills: []model.OOMKill{{Time: now, Source: "oomd", Unit: "example.service"}}}
	if err := f.store.RecordObservation(f.machine.id, b, now); err != nil {
		t.Fatal(err)
	}
	path := "/v1/operator/machines/" + f.machine.id + "/oom-kills"
	rec := operatorRequest(t, f.mux, http.MethodGet, path, "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "example.service") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read status %d", rec.Code)
	}
	if rec := operatorRequest(t, f.mux, http.MethodGet, path+"?raw=true", "", ""); rec.Code != 400 {
		t.Fatalf("query status %d", rec.Code)
	}
	if rec := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/machines/missing/oom-kills", "", ""); rec.Code != 404 {
		t.Fatalf("missing status %d", rec.Code)
	}
}

func TestOOMRouteRequiresView(t *testing.T) {
	f := newJobsFixture(t, "example-machine")
	(&hub{store: f.store}).operatorRoutes(f.mux)
	authorizer := &boundaryAuthorizer{decision: operatorauth.Decision{HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired, Detail: "view capability required"}}
	boundary := newOperatorBoundary(f.mux, authorizer, f.store, operatorRoutePolicies, testOperatorAuthority)
	rec := httptest.NewRecorder()
	boundary.ServeHTTP(rec, newBoundaryRequest(http.MethodGet, "/v1/operator/machines/"+f.machine.id+"/oom-kills", nil))
	assertAPIError(t, rec, http.StatusForbidden, string(operatorauth.CapabilityRequired))
	if calls := authorizer.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.View {
		t.Fatal("missing View authorization")
	}
}
func TestOOMObservationUpload(t *testing.T) {
	f := newJobsFixture(t, "example-machine")
	now := time.Now().UTC()
	b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	post(t, f.mux, "/v1/observations:batch", f.machine.token, raw, http.StatusNoContent)
	b.OOMKills = []model.OOMKill{{Time: now, Source: "kernel", Process: "worker"}}
	raw, err = json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	post(t, f.mux, "/v1/observations:batch", f.machine.token, raw, http.StatusNoContent)
	events, err := f.store.RecentOOMKills(f.machine.id, time.Now().UTC())
	if err != nil || len(events) != 1 {
		t.Fatal("upload did not retain evidence")
	}
	b.OOMKills[0].Process = "worker password=private"
	raw, err = json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	post(t, f.mux, "/v1/observations:batch", f.machine.token, raw, http.StatusBadRequest)
}
