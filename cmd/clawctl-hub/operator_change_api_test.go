package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorChangesUsesSafeHubClockReadWithoutMutation(t *testing.T) {
	st := boundaryStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	machineID := "machine-change-api"
	if err := st.UpsertMachine(store.Machine{
		MachineID: machineID, DisplayName: "SampleHub Changes", Expected: true,
		CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	before := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(72 * time.Hour),
		Credentials: []model.Credential{{
			Provider: "claude", Status: model.CredConfigured, ActiveAccountID: "secret-account-before",
		}},
	}
	after := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-72 * time.Hour),
		Credentials: []model.Credential{{
			Provider: "claude", Status: model.CredExpired, ActiveAccountID: "secret-account-after",
		}},
	}
	from := now.Add(-2 * time.Hour)
	if err := st.RecordObservation(machineID, before, from.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	changedAt := now.Add(-time.Hour)
	if err := st.RecordObservation(machineID, after, changedAt); err != nil {
		t.Fatal(err)
	}
	var rowsBefore int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM observed_state`).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	path := "/v1/operator/changes?machine_id=" + machineID + "&kind=credential&subject=claude" +
		"&from=" + url.QueryEscape(from.Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(now.Format(time.RFC3339)) + "&limit=10"
	response := operatorRequest(t, mux, http.MethodGet, path, "", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("ETag") != "" || response.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var result operator.ChangeListResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != operator.ChangeReadSchemaVersion || result.Consistency != operator.ChangeReadConsistency ||
		result.Total != 1 || result.MatchedTotal != 1 || len(result.Items) != 1 || result.Items[0].Kind != operator.ChangeKindCredential ||
		!result.Items[0].ChangedAt.Equal(changedAt) || result.Items[0].Before == nil ||
		result.Items[0].Before.Status == nil || *result.Items[0].Before.Status != model.CredConfigured ||
		result.Items[0].After == nil || result.Items[0].After.Status == nil ||
		*result.Items[0].After.Status != model.CredExpired {
		t.Fatalf("result=%+v", result)
	}
	for _, forbidden := range []string{"secret-account-before", "secret-account-after", "active_account_id"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("safe changes response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	var rowsAfter int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM observed_state`).Scan(&rowsAfter); err != nil || rowsAfter != rowsBefore {
		t.Fatalf("GET mutated observations: %d -> %d err=%v", rowsBefore, rowsAfter, err)
	}
}

func TestOperatorChangesRejectsAmbiguousOrNoncanonicalQueries(t *testing.T) {
	for _, suffix := range []string{
		"?", "?unknown=value", "?machine_id=a&machine_id=b", "?machine_id=%20a",
		"?kind=unknown", "?kind=state&kind=state", "?subject=", "?limit=0",
		"?limit=101", "?limit=01", "?limit=%2B1", "?cursor=not-base64url",
		"?from=2026-09-08", "?from=2026-09-08T12%3A00%3A00.000Z",
		"?from=2026-09-08T13%3A00%3A00Z&to=2026-09-08T12%3A00%3A00Z",
		"?from=2026-08-01T00%3A00%3A00Z&to=2026-09-08T00%3A00%3A00Z",
	} {
		t.Run(suffix, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/operator/changes"+suffix, nil)
			if _, err := parseOperatorChangeListRequest(request); err == nil {
				t.Fatalf("query %q was accepted", suffix)
			}
		})
	}
}

func TestOperatorChangesParsesUTCNormalizedWindowAndKinds(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet,
		"/v1/operator/changes?kind=state&kind=credential&from=2026-09-08T08%3A00%3A00-04%3A00&to=2026-09-08T13%3A00%3A00Z", nil)
	parsed, err := parseOperatorChangeListRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.From == nil || parsed.To == nil || parsed.From.Format(time.RFC3339) != "2026-09-08T12:00:00Z" ||
		parsed.To.Format(time.RFC3339) != "2026-09-08T13:00:00Z" || len(parsed.Kinds) != 2 {
		t.Fatalf("request=%+v", parsed)
	}
}

func TestOperatorChangesDoesNotReflectBadCursor(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	secret := "do-not-reflect-change-secret"
	response := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/changes?cursor="+secret, "", "")
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOperatorChangesRedactsUnknownCredentialSubjectEverywhere(t *testing.T) {
	st := boundaryStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	const privateSubject = "alice@example.com"
	const machineID = "machine-private-change-subject"
	if err := st.UpsertMachine(store.Machine{
		MachineID: machineID, DisplayName: "Private Subject", Expected: true,
		CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for index, status := range []model.CredStatus{model.CredConfigured, model.CredExpired} {
		batch := model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(time.Duration(index-2) * time.Hour),
			Credentials: []model.Credential{{Provider: privateSubject, Status: status}},
		}
		if err := st.RecordObservation(machineID, batch, now.Add(time.Duration(index-2)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	response := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/changes?machine_id="+machineID+"&kind=credential&limit=1", "", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), privateSubject) {
		t.Fatalf("status=%d private subject leaked: %s", response.Code, response.Body.String())
	}
	var result operator.ChangeListResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Subject != "(redacted subject)" ||
		!containsString(result.Items[0].Issues, "subject_not_allowlisted") ||
		!containsString(result.Items[0].AlteredFields, "subject") {
		t.Fatalf("unsafe subject projection: %+v", result.Items)
	}
	if result.NextCursor != nil && strings.Contains(*result.NextCursor, privateSubject) {
		t.Fatalf("cursor leaked private subject: %q", *result.NextCursor)
	}

	rejected := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/changes?subject="+url.QueryEscape(privateSubject), "", "")
	if rejected.Code != http.StatusBadRequest || strings.Contains(rejected.Body.String(), privateSubject) {
		t.Fatalf("status=%d rejected filter reflected private subject: %s", rejected.Code, rejected.Body.String())
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestOperatorChangeReadTypedErrorsHaveStableHTTPContract(t *testing.T) {
	for _, test := range []struct {
		name, code string
		err        error
		status     int
		retry      bool
	}{
		{"invalid", "BAD_REQUEST", operator.ErrInvalidChangeRead, http.StatusBadRequest, false},
		{"gone", "CHANGE_TRAVERSAL_GONE", operator.ErrChangeReadTraversalGone, http.StatusGone, false},
		{"broad", "CHANGE_READ_TOO_BROAD", operator.ErrChangeReadTooBroad, http.StatusUnprocessableEntity, false},
		{"busy", "CHANGE_READ_BUSY", operator.ErrChangeReadBusy, http.StatusTooManyRequests, true},
		{"timeout", "CHANGE_READ_TIMEOUT", operator.ErrChangeReadTimedOut, http.StatusServiceUnavailable, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if !writeOperatorChangeReadError(recorder, test.err) || recorder.Code != test.status ||
				!strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) ||
				(recorder.Header().Get("Retry-After") != "") != test.retry {
				t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
	if writeOperatorChangeReadError(httptest.NewRecorder(), errTestUnexpectedChangeRead) {
		t.Fatal("unexpected internal error was claimed as a typed change-read error")
	}
}

var errTestUnexpectedChangeRead = &url.Error{Op: "test", URL: "changes", Err: http.ErrServerClosed}
