package operatorclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
)

func changeClientTestResult() operator.ChangeListResult {
	evaluatedAt := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	from, to := evaluatedAt.Add(-operator.DefaultChangeWindow), evaluatedAt
	machineID := "machine-1"
	machineDigest := sha256.Sum256([]byte("change-machine-v1\x00" + machineID))
	before, after := state.Degraded, state.Online
	severity := after.Severity()
	result := operator.ChangeListResult{
		SchemaVersion: operator.ChangeReadSchemaVersion, Consistency: operator.ChangeReadConsistency,
		EvaluatedAt: evaluatedAt,
		Window: operator.ChangeWindow{
			From: from, To: to, Boundary: "(from,to]", TimeBasis: "hub_received_at",
			MaximumSeconds: int64(operator.MaxChangeWindow / time.Second),
		},
		CreationCeilings: operator.ChangeCreationCeilings{StateHistory: 7},
		Total:            1, MatchedTotal: 1,
		KindCounts: changeClientTestKindCounts(operator.ChangeKindState, 1),
		Coverage: operator.ChangeCoverage{
			ObservationComparison: "endpoint_delta", ObservationHistory: "complete",
			RegistryHistory: "complete", RegistryHistoryStartedAt: timePointer(evaluatedAt.Add(-48 * time.Hour)),
			StateHistory: "complete", StateHistoryStartedAt: timePointer(evaluatedAt.Add(-48 * time.Hour)),
			Issues: []string{},
		},
		Items: []operator.ChangeItem{{
			ChangeID:   "sha256:" + strings.Repeat("a", 64),
			MachineRef: "sha256:" + hex.EncodeToString(machineDigest[:]), MachineID: &machineID,
			DisplayName: "cnode", Kind: operator.ChangeKindState, Subject: "state",
			Semantics: "transition", ChangedAt: evaluatedAt.Add(-time.Hour), Severity: &severity,
			BaselineStatus: "known",
			Before:         &operator.ChangeValue{State: &before}, After: &operator.ChangeValue{State: &after},
			ChangedFields: []string{"state"}, RedactedFields: []string{},
			Issues: []string{}, AlteredFields: []string{},
		}},
	}
	return result
}

func changeClientTestKindCounts(nonzero string, count int) []operator.ChangeKindCount {
	result := make([]operator.ChangeKindCount, 0, len(operator.ChangeKinds()))
	for _, kind := range operator.ChangeKinds() {
		value := 0
		if kind == nonzero {
			value = count
		}
		result = append(result, operator.ChangeKindCount{Kind: kind, Count: value})
	}
	return result
}

func changeClientHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
}

func timePointer(value time.Time) *time.Time { return &value }

func TestChangesClientAcceptsCanonicalFilteredReadAndQuery(t *testing.T) {
	result := changeClientTestResult()
	result.Coverage.ObservationHistory = operator.ChangeCoverageNotApplicable
	result.Coverage.RegistryHistory = operator.ChangeCoverageNotApplicable
	result.Coverage.RegistryHistoryStartedAt = nil
	from := result.Window.From.In(time.FixedZone("EDT", -4*60*60))
	to := result.Window.To.In(time.FixedZone("EDT", -4*60*60))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/operator/changes" ||
			r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request=%s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		query := r.URL.Query()
		if query.Get("machine_id") != "machine-1" || query.Get("subject") != "state" ||
			strings.Join(query["kind"], ",") != operator.ChangeKindState ||
			query.Get("from") != result.Window.From.Format(time.RFC3339) ||
			query.Get("to") != result.Window.To.Format(time.RFC3339) || query.Get("limit") != "7" {
			t.Errorf("query=%v", query)
		}
		changeClientHeaders(w)
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()

	got, err := operatorClientForServer(t, server).Changes(t.Context(), operator.ChangeListRequest{
		MachineID: "machine-1", Kinds: []string{operator.ChangeKindState}, Subject: "state",
		From: &from, To: &to, Limit: 7,
	})
	if err != nil || len(got.Items) != 1 || got.Items[0].Kind != operator.ChangeKindState {
		t.Fatalf("changes=%+v err=%v", got, err)
	}
}

func TestChangesClientCanonicalizesRepeatedKindOrder(t *testing.T) {
	result := changeClientTestResult()
	result.Coverage.RegistryHistory = operator.ChangeCoverageNotApplicable
	result.Coverage.RegistryHistoryStartedAt = nil
	result.Total, result.MatchedTotal = 0, 0
	result.KindCounts = changeClientTestKindCounts("", 0)
	result.Items = []operator.ChangeItem{}
	result.CreationCeilings = operator.ChangeCreationCeilings{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := strings.Join(r.URL.Query()["kind"], ","); got != "state,systemd" {
			t.Errorf("kind order=%q", got)
		}
		changeClientHeaders(w)
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	if _, err := operatorClientForServer(t, server).ListChanges(t.Context(), operator.ChangeListRequest{
		Kinds: []string{operator.ChangeKindSystemd, operator.ChangeKindState},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChangesClientRequiresCoverageToMatchSelectedSources(t *testing.T) {
	evaluatedAt := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	trackedFrom := evaluatedAt.Add(-48 * time.Hour)
	coverage := func(observation, registry, stateHistory string) operator.ChangeCoverage {
		value := operator.ChangeCoverage{
			ObservationComparison: "endpoint_delta", ObservationHistory: observation,
			RegistryHistory: registry, StateHistory: stateHistory, Issues: []string{},
		}
		if registry != operator.ChangeCoverageNotApplicable {
			value.RegistryHistoryStartedAt = timePointer(trackedFrom)
		}
		if stateHistory != operator.ChangeCoverageNotApplicable {
			value.StateHistoryStartedAt = timePointer(trackedFrom)
		}
		return value
	}
	for _, test := range []struct {
		name     string
		request  operator.ChangeListRequest
		coverage operator.ChangeCoverage
		ok       bool
	}{
		{
			name: "unfiltered reads all", coverage: coverage("complete", "complete", "complete"), ok: true,
		},
		{
			name: "state only", request: operator.ChangeListRequest{Kinds: []string{operator.ChangeKindState}},
			coverage: coverage(operator.ChangeCoverageNotApplicable, operator.ChangeCoverageNotApplicable, "complete"), ok: true,
		},
		{
			name: "observation only", request: operator.ChangeListRequest{Kinds: []string{operator.ChangeKindCredential}},
			coverage: coverage("complete", operator.ChangeCoverageNotApplicable, operator.ChangeCoverageNotApplicable), ok: true,
		},
		{
			name: "fixed subject excludes registry", request: operator.ChangeListRequest{
				Kinds: []string{operator.ChangeKindRegistry}, Subject: "state",
			},
			coverage: coverage(operator.ChangeCoverageNotApplicable, operator.ChangeCoverageNotApplicable,
				operator.ChangeCoverageNotApplicable), ok: true,
		},
		{
			name: "applicable source marked n a", coverage: coverage(operator.ChangeCoverageNotApplicable, "complete", "complete"),
		},
		{
			name: "unread source marked complete", request: operator.ChangeListRequest{Kinds: []string{operator.ChangeKindState}},
			coverage: coverage("complete", operator.ChangeCoverageNotApplicable, "complete"),
		},
		{
			name: "n a observation carries prune evidence", request: operator.ChangeListRequest{Kinds: []string{operator.ChangeKindState}},
			coverage: func() operator.ChangeCoverage {
				value := coverage(operator.ChangeCoverageNotApplicable, operator.ChangeCoverageNotApplicable, "complete")
				value.ObservationRowsPruned = 1
				return value
			}(),
		},
		{
			name: "n a registry carries tracking evidence", request: operator.ChangeListRequest{Kinds: []string{operator.ChangeKindState}},
			coverage: func() operator.ChangeCoverage {
				value := coverage(operator.ChangeCoverageNotApplicable, operator.ChangeCoverageNotApplicable, "complete")
				value.RegistryHistoryStartedAt = timePointer(trackedFrom)
				return value
			}(),
		},
		{
			name: "n a state carries tracking evidence", request: operator.ChangeListRequest{Kinds: []string{operator.ChangeKindCredential}},
			coverage: func() operator.ChangeCoverage {
				value := coverage("complete", operator.ChangeCoverageNotApplicable, operator.ChangeCoverageNotApplicable)
				value.StateHistoryStartedAt = timePointer(trackedFrom)
				return value
			}(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateChangeCoverage(test.coverage, evaluatedAt, test.request)
			if (err == nil) != test.ok {
				t.Fatalf("accepted=%t want=%t err=%v coverage=%+v", err == nil, test.ok, err, test.coverage)
			}
		})
	}
}

func changeClientSparseObservationResult(kind string, after *operator.ChangeValue,
	changedFields, redactedFields, issues []string,
) operator.ChangeListResult {
	result := changeClientTestResult()
	result.CreationCeilings = operator.ChangeCreationCeilings{Observations: 9}
	result.KindCounts = changeClientTestKindCounts(kind, 1)
	item := &result.Items[0]
	item.Kind, item.Subject, item.Semantics = kind, kind, "window_comparison"
	if kind == operator.ChangeKindSystemd {
		item.Subject = "clawctl-agent.service"
	}
	item.Severity, item.Before, item.After = nil, nil, after
	item.BaselineStatus = "not_observed"
	item.ChangedFields = changedFields
	item.RedactedFields = redactedFields
	item.Issues = issues
	return result
}

func TestChangesClientAcceptsSparseFirstObservationDTO(t *testing.T) {
	present := true
	result := changeClientSparseObservationResult(operator.ChangeKindOpenClaw,
		&operator.ChangeValue{Present: &present}, []string{"present"},
		[]string{"absence_reason", "installation_details"}, []string{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		changeClientHeaders(w)
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	got, err := operatorClientForServer(t, server).Changes(t.Context(), operator.ChangeListRequest{})
	if err != nil || len(got.Items) != 1 || got.Items[0].Before != nil ||
		got.Items[0].After == nil || got.Items[0].After.CLIVersion != nil {
		t.Fatalf("sparse changes=%+v err=%v", got, err)
	}
}

func TestChangesClientRequiresIssueForNullSystemdRestarts(t *testing.T) {
	present := true
	canonical := changeClientSparseObservationResult(operator.ChangeKindSystemd,
		&operator.ChangeValue{Present: &present}, []string{"present"},
		[]string{"process_identity"}, []string{"after_restarts_not_canonical"})
	for _, test := range []struct {
		name   string
		mutate func(*operator.ChangeListResult)
		ok     bool
	}{
		{name: "issue bound null", ok: true},
		{name: "missing issue", mutate: func(value *operator.ChangeListResult) {
			value.Items[0].Issues = []string{}
		}},
		{name: "negative still rejected", mutate: func(value *operator.ChangeListResult) {
			negative := -1
			value.Items[0].After.Restarts = &negative
			value.Items[0].ChangedFields = []string{"present", "restarts"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := canonical
			value.Items = append([]operator.ChangeItem(nil), canonical.Items...)
			if test.mutate != nil {
				test.mutate(&value)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				changeClientHeaders(w)
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			_, err := operatorClientForServer(t, server).Changes(t.Context(), operator.ChangeListRequest{})
			if (err == nil) != test.ok {
				t.Fatalf("accepted=%t want=%t err=%v value=%+v", err == nil, test.ok, err, value)
			}
		})
	}
}

// TestChangesClientValidatesSystemdMeasurementThreeStates 釘住官方 strict client 接受的 systemd 三態組合。
func TestChangesClientValidatesSystemdMeasurementThreeStates(t *testing.T) {
	if operator.ChangeReadSchemaVersion != 2 {
		t.Fatalf("ChangeReadSchemaVersion=%d, want 2", operator.ChangeReadSchemaVersion)
	}
	trueValue, falseValue, zero := true, false, 0
	tests := []struct {
		name  string
		value operator.ChangeValue
		ok    bool
	}{
		{name: "未量到", value: operator.ChangeValue{Measured: &falseValue}, ok: true},
		{name: "空 present 但 measured true", value: operator.ChangeValue{Measured: &trueValue, Restarts: &zero}},
		{name: "空 present 且 measured nil", value: operator.ChangeValue{Restarts: &zero}},
		{name: "存在卻帶 measured", value: operator.ChangeValue{Present: &trueValue, Measured: &trueValue, Restarts: &zero}},
		{name: "不存在卻沒帶 measured", value: operator.ChangeValue{Present: &falseValue, Restarts: &zero}},
		{name: "未量到卻帶 restarts", value: operator.ChangeValue{Measured: &falseValue, Restarts: &zero}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateChangeValue(operator.ChangeKindSystemd, &test.value, "after", []string{})
			if (err == nil) != test.ok {
				t.Fatalf("接受狀態=%t，預期=%t，錯誤=%v，值=%+v", err == nil, test.ok, err, test.value)
			}
		})
	}
}

func TestChangesClientAcceptsIssueBoundMalformedKnownTransitionBaseline(t *testing.T) {
	result := changeClientTestResult()
	result.Items[0].Before = nil
	result.Items[0].Issues = []string{"before_state_not_canonical"}
	for _, test := range []struct {
		name string
		ok   bool
	}{
		{name: "issue bound", ok: true},
		{name: "missing issue"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := result
			value.Items = append([]operator.ChangeItem(nil), result.Items...)
			if !test.ok {
				value.Items[0].Issues = []string{}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				changeClientHeaders(w)
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			_, err := operatorClientForServer(t, server).Changes(t.Context(), operator.ChangeListRequest{})
			if (err == nil) != test.ok {
				t.Fatalf("accepted=%t want=%t err=%v", err == nil, test.ok, err)
			}
		})
	}
}

func TestChangesClientFailClosesCredentialSubjectDisclosureAndAcceptsConstantRedaction(t *testing.T) {
	status := model.CredConfigured
	base := changeClientSparseObservationResult(operator.ChangeKindCredential,
		&operator.ChangeValue{Status: &status}, []string{"status"}, []string{"account_identity"}, []string{})
	base.Items[0].Subject = "alice@example.com"
	for _, test := range []struct {
		name    string
		request operator.ChangeListRequest
		mutate  func(*operator.ChangeListResult)
		ok      bool
	}{
		{name: "plaintext disclosure"},
		{name: "digest remains correlatable",
			mutate: func(value *operator.ChangeListResult) {
				value.Items[0].Subject = "sha256:" + strings.Repeat("c", 64)
				value.Items[0].Issues = []string{"subject_not_allowlisted"}
				value.Items[0].AlteredFields = []string{"subject"}
			}},
		{name: "constant redaction with evidence",
			mutate: func(value *operator.ChangeListResult) {
				value.Items[0].Subject = "(redacted subject)"
				value.Items[0].Issues = []string{"subject_not_allowlisted"}
				value.Items[0].AlteredFields = []string{"subject"}
			}, ok: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.Items = append([]operator.ChangeItem(nil), base.Items...)
			if test.mutate != nil {
				test.mutate(&value)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				changeClientHeaders(w)
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			_, err := operatorClientForServer(t, server).Changes(t.Context(), test.request)
			if (err == nil) != test.ok {
				t.Fatalf("accepted=%t want=%t err=%v subject=%q", err == nil, test.ok, err, value.Items[0].Subject)
			}
		})
	}
}

func TestChangesClientPagesWithBoundCursor(t *testing.T) {
	first := changeClientTestResult()
	first.Total, first.MatchedTotal = 2, 2
	first.KindCounts = changeClientTestKindCounts(operator.ChangeKindState, 2)
	first.CreationCeilings.StateHistory = 10
	firstItem := first.Items[0]
	cursor := changeClientTestCursor(t, operator.ChangeListRequest{Limit: 1}, first,
		changeClientPosition{At: firstItem.ChangedAt, SourceRank: 1, SourceRowID: 10, ChangeID: firstItem.ChangeID})
	first.NextCursor = &cursor

	second := first
	secondItem := firstItem
	secondItem.ChangeID = "sha256:" + strings.Repeat("b", 64)
	secondItem.ChangedAt = firstItem.ChangedAt.Add(-time.Minute)
	second.Items = []operator.ChangeItem{secondItem}
	second.NextCursor = nil
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		changeClientHeaders(w)
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(first)
		} else if r.URL.Query().Get("cursor") == cursor {
			_ = json.NewEncoder(w).Encode(second)
		} else {
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	request := operator.ChangeListRequest{Limit: 1}
	gotFirst, err := client.Changes(t.Context(), request)
	if err != nil || gotFirst.NextCursor == nil || *gotFirst.NextCursor != cursor {
		t.Fatalf("first=%+v err=%v", gotFirst, err)
	}
	request.Cursor = *gotFirst.NextCursor
	gotSecond, err := client.Changes(t.Context(), request)
	if err != nil || gotSecond.NextCursor != nil || len(gotSecond.Items) != 1 ||
		gotSecond.Items[0].ChangeID != secondItem.ChangeID || calls.Load() != 2 {
		t.Fatalf("second=%+v calls=%d err=%v", gotSecond, calls.Load(), err)
	}
}

func changeClientTestCursor(t *testing.T, request operator.ChangeListRequest, result operator.ChangeListResult,
	after changeClientPosition,
) string {
	t.Helper()
	_, normalized, err := encodeChangeListQuery(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(changeClientCursor{
		Version: operator.ChangeReadSchemaVersion, FilterDigest: changeClientFilterDigest(normalized),
		EvaluatedAt: result.EvaluatedAt, From: result.Window.From, To: result.Window.To,
		Ceilings: result.CreationCeilings, After: after,
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestChangesClientRejectsInvalidRequestsBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	equal := now
	tooOld := now.Add(-operator.MaxChangeWindow - time.Second)
	nanos := now.Add(time.Nanosecond)
	for _, request := range []operator.ChangeListRequest{
		{MachineID: " machine"}, {Subject: "bad\nsubject"}, {Subject: "alice@example.com"},
		{Subject: "sha256:" + strings.Repeat("a", 64)}, {Kinds: []string{"unknown"}},
		{Kinds: []string{operator.ChangeKindState, operator.ChangeKindState}},
		{From: &equal, To: &equal}, {From: &tooOld, To: &now}, {From: &nanos},
		{Limit: -1}, {Limit: operator.MaxChangeReadLimit + 1}, {Cursor: "not-a-cursor"},
	} {
		if got, err := client.Changes(t.Context(), request); err == nil {
			t.Errorf("accepted request %+v: %+v", request, got)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached network %d times", calls.Load())
	}
}

func TestChangesClientRejectsUnsafeHeadersAndContradictoryDocuments(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		header func(http.Header)
		mutate func(*operator.ChangeListResult)
		raw    func([]byte) []byte
	}{
		{name: "wrong success status", status: http.StatusCreated},
		{name: "etag", header: func(h http.Header) { h.Set("ETag", `"changes"`) }},
		{name: "replay", header: func(h http.Header) { h.Set("Idempotency-Replayed", "true") }},
		{name: "wrong consistency", mutate: func(v *operator.ChangeListResult) { v.Consistency = "snapshot" }},
		{name: "matched total mismatch", mutate: func(v *operator.ChangeListResult) { v.MatchedTotal = 0 }},
		{name: "kind total mismatch", mutate: func(v *operator.ChangeListResult) { v.KindCounts[0].Count++ }},
		{name: "unsafe display", mutate: func(v *operator.ChangeListResult) { v.Items[0].DisplayName = "line1\nline2" }},
		{name: "machine ref mismatch", mutate: func(v *operator.ChangeListResult) { v.Items[0].MachineRef = "sha256:" + strings.Repeat("c", 64) }},
		{name: "changed fields mismatch", mutate: func(v *operator.ChangeListResult) { v.Items[0].ChangedFields = []string{} }},
		{name: "unknown top field", raw: func(raw []byte) []byte {
			return []byte(strings.TrimSuffix(string(raw), "}") + `,"raw_payload":"secret"}`)
		}},
		{name: "duplicate top field", raw: func(raw []byte) []byte {
			return []byte(strings.TrimSuffix(string(raw), "}") + `,"total":1}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := changeClientTestResult()
			if test.mutate != nil {
				test.mutate(&value)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if test.raw != nil {
				raw = test.raw(raw)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				changeClientHeaders(w)
				if test.header != nil {
					test.header(w.Header())
				}
				status := test.status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			if got, err := operatorClientForServer(t, server).Changes(t.Context(), operator.ChangeListRequest{}); err == nil {
				t.Fatalf("accepted contradictory response: %+v", got)
			}
		})
	}
}

func TestChangesClientBoundsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		changeClientHeaders(w)
		_, _ = w.Write([]byte(strings.Repeat(" ", int(maxChangeResponseBytes)+1)))
	}))
	defer server.Close()
	if got, err := operatorClientForServer(t, server).Changes(t.Context(), operator.ChangeListRequest{}); err == nil {
		t.Fatalf("accepted oversized response: %+v", got)
	}
}
