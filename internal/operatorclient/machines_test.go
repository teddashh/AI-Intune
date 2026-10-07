package operatorclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
)

const validMachineSummaryJSON = `{
  "machine_id":"machine-1",
  "display_name":"cnode",
  "expected":true,
  "created_at":"2026-09-07T12:00:00Z",
  "enrolled_at":"2026-09-07T12:01:00Z",
  "retired_at":null,
  "channel":null,
  "channel_revision":0,
  "state":"Online",
  "state_since":null,
  "reporting":true,
  "last_checkin_received_at":"2026-09-07T12:02:00Z",
  "last_observation_received_at":null,
  "issues":[],
  "altered_fields":[]
}`

func validMachineListJSON(item string) string {
	return machineListPageJSON([]string{item}, 1, 1, 1, nil)
}

func machineListPageJSON(items []string, creationCeiling int64, total, matchedTotal int, nextCursor *string) string {
	next := "null"
	if nextCursor != nil {
		raw, _ := json.Marshal(*nextCursor)
		next = string(raw)
	}
	return fmt.Sprintf(`{
  "schema_version":2,
  "consistency":"registry_creation_ceiling_with_live_health",
  "evaluated_at":"2026-09-07T12:02:30.987654321Z",
  "creation_ceiling":%d,
  "total":%d,
  "matched_total":%d,
  "active":%d,
  "retired":0,
  "expected":%d,
  "reporting":%d,
  "state_counts":[
    {"state":"Online","count":%d},{"state":"Degraded","count":0},
    {"state":"Unreachable","count":0},{"state":"NeverReported","count":0},
    {"state":"IdentityConflict","count":0}
  ],
  "denominator_state_counts":[
    {"state":"Online","count":%d},{"state":"Degraded","count":0},
    {"state":"Unreachable","count":0},{"state":"NeverReported","count":0},
    {"state":"IdentityConflict","count":0}
  ],
  "items":[%s],
  "next_cursor":%s
}`, creationCeiling, total, matchedTotal, total, total, total, total, total,
		strings.Join(items, ","), next)
}

func validMachineDetailJSON(item string) string {
	var identity struct {
		MachineID   string     `json:"machine_id"`
		DisplayName string     `json:"display_name"`
		RetiredAt   *time.Time `json:"retired_at"`
	}
	if err := json.Unmarshal([]byte(item), &identity); err != nil {
		panic(err)
	}
	machineID, _ := json.Marshal(identity.MachineID)
	displayName, _ := json.Marshal(identity.DisplayName)
	affectsFleetState := identity.RetiredAt == nil
	return fmt.Sprintf(`{
	"schema_version":6,
  "evaluated_at":"2026-09-07T12:02:30.987654321Z",
  "item":%s,
  "judgement":{
    "state":"Online","reason":{"text":"fixture judgement","max_bytes":4096,"bytes":17,"truncated":false,"issues":[]},
    "affects_fleet_state":%t,"findings":{"total":0,"invalid":0,"truncated":false,"items":[]}
  },
  "expectations":{"configured":false,"read_failed":false,"error":null,
    "rules":{"total":0,"invalid":0,"truncated":false,"items":[]}},
  "disclosure":{
    "checkin_limit":1000,"checkin_window_seconds":86400,"state_history_limit":20,
    "identity_hint_limit":100,"finding_limit":100,"expectation_limit":50,"event_type_limit":20,
    "max_text_bytes":4096,
    "observer_producer":{"kind":"observer_agent","machine_id":%s,"display_name":%s,"authority":"machine_bearer"},
    "independent_verifier":false,"liveness_uses_received_at":true,"agent_sent_at_is_liveness":false,
    "host_identity_included":true,"tailscale_ip_included":true,"connect_coordinates_excluded":true,
    "pending_enrollment_excluded":true,"expectation_display_definitions_included":true,
    "expectation_config_path_field_excluded":true,"expectation_parser_fields_excluded":true,
    "artifact_paths_included":true,"artifact_content_inspected":false,
    "artifact_freshness_is_work_outcome":false,"event_failure_types_operator_declared":true,
    "event_content_keyword_scanning":false,"expectation_text_path_redacted_by_hub":false,
    "expectation_text_secret_redacted_by_hub":false,
    "judgement_derived_by_hub":true,"judgement_may_use_unverified_input":true,
    "judgement_text_parsed_by_hub":false,"judgement_text_path_redacted_by_hub":false,
    "judgement_text_secret_redacted_by_hub":false,"known_secret_fields_excluded":true,
    "state_reason_parsed_by_hub":false,"state_reason_path_redacted_by_hub":false,
    "complete_history_claimed":false,"identity_hints_non_expiring":true
  },
  "monitor":{
    "ever_checked_in":true,"last_checkin_received_at":"2026-09-07T12:02:00Z",
    "checkin_interval_seconds":120,"clock_skew_seconds":0,"agent_version":null,
    "distinct_agent_starts_1h":null,"distinct_boot_ids_1h":null,"agent_unit_n_restarts":null,
    "linger_enabled":null,"last_observation_received_at":null
  },
  "checkins":{"rows_seen":1,"invalid":0,"truncated":false,"items":[{
    "sent_at":"2026-09-07T12:02:00Z","received_at":"2026-09-07T12:02:00Z",
    "agent_version":null,"boot_id":null,"agent_seq":null,"uptime_seconds":null,
    "disk_free_bytes":null,"disk_total_bytes":null,"observation_age_seconds":null,"clock_skew_seconds":null
  }]},
  "state_history":{"rows_seen":0,"invalid":0,"truncated":false,"items":[]},
  "identity":{"observed":false,"decoded":false,"observed_at":null,"value":null},
  "identity_hints":{"total":0,"invalid":0,"truncated":false,"items":[]},
  "resources":{"observed":false,"decoded":false,"invalid":false,"observed_at":null,"value":null}
}`, item, affectsFleetState, machineID, displayName)
}

func machineSummaryJSON(machineID, displayName, createdAt string) string {
	return strings.NewReplacer(
		`"machine_id":"machine-1"`, `"machine_id":"`+machineID+`"`,
		`"display_name":"cnode"`, `"display_name":"`+displayName+`"`,
		`"created_at":"2026-09-07T12:00:00Z"`, `"created_at":"`+createdAt+`"`,
	).Replace(validMachineSummaryJSON)
}

func machineClientTestCursor(t *testing.T, request operator.MachineListRequest, creationCeiling int64,
	createdAt time.Time, machineID string,
) string {
	t.Helper()
	_, normalized, err := encodeMachineListQuery(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(machineClientCursor{
		Version: operator.MachineListReadSchemaVersion, FilterDigest: machineClientFilterDigest(normalized),
		CreationCeiling: creationCeiling,
		After:           operator.MachineReadPosition{CreatedAt: createdAt.UTC(), MachineID: machineID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func machineClientEncodedCursor(t *testing.T, cursor machineClientCursor) string {
	t.Helper()
	raw, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestMachineReadClientAcceptsCanonicalListAndDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "private, no-store")
		switch r.URL.Path {
		case "/v1/operator/machines":
			_, _ = w.Write([]byte(validMachineListJSON(validMachineSummaryJSON)))
		case "/v1/operator/machines/machine-1":
			_, _ = w.Write([]byte(validMachineDetailJSON(validMachineSummaryJSON)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	list, err := client.Machines(t.Context())
	if err != nil || len(list.Items) != 1 || list.Items[0].MachineID != "machine-1" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if _, ok := list.StoreOverview(); ok {
		t.Fatal("HTTP-decoded list falsely exposed an in-process Store overview")
	}
	detail, err := client.Machine(t.Context(), "machine-1")
	if err != nil || detail.Item.MachineID != "machine-1" || detail.Judgement.Findings.Items == nil {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	// received_at is stored at second precision. Queued check-ins may share a
	// Hub receive second even though their agent sent times differ.
	second := detail.Checkins.Items[0]
	second.SentAt = second.SentAt.Add(-time.Minute)
	detail.Checkins.Items = append(detail.Checkins.Items, second)
	detail.Checkins.RowsSeen++
	if err := validateMachineDetailResult(detail, "machine-1"); err != nil {
		t.Fatalf("legitimate same-second check-ins were rejected: %v", err)
	}
}

func TestMachineReadClientIncludesActiveLegacyExpectedFalseInDenominator(t *testing.T) {
	body := validMachineListJSON(strings.Replace(validMachineSummaryJSON,
		`"expected":true`, `"expected":false`, 1))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	result, err := operatorClientForServer(t, server).Machines(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Active != 1 || result.Expected != 1 || len(result.Items) != 1 || result.Items[0].Expected {
		t.Fatalf("legacy expected field changed lifecycle denominator: %+v", result)
	}
}

func TestMachineReadClientRejectsMalformedTypedDetailSections(t *testing.T) {
	canonical := validMachineDetailJSON(validMachineSummaryJSON)
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown nested monitor field",
			body: strings.Replace(canonical, `"ever_checked_in":true`, `"ever_checked_in":true,"private_path":"/tmp/x"`, 1),
			want: "unknown field",
		},
		{
			name: "unknown nested judgement field",
			body: strings.Replace(canonical, `"affects_fleet_state":true`,
				`"affects_fleet_state":true,"raw_reason":"no"`, 1),
			want: "unknown field",
		},
		{
			name: "active judgement detached from fleet state",
			body: strings.Replace(canonical, `"affects_fleet_state":true`,
				`"affects_fleet_state":false`, 1),
			want: "does not match fleet state",
		},
		{
			name: "finding page total mismatch",
			body: strings.Replace(canonical,
				`"findings":{"total":0,"invalid":0,"truncated":false,"items":[]}`,
				`"findings":{"total":1,"invalid":0,"truncated":false,"items":[]}`, 1),
			want: "finding page metadata",
		},
		{
			name: "false liveness clock disclosure",
			body: strings.Replace(canonical, `"liveness_uses_received_at":true`, `"liveness_uses_received_at":false`, 1),
			want: "disclosure metadata",
		},
		{
			name: "checkin row count mismatch",
			body: strings.Replace(canonical, `"checkins":{"rows_seen":1`, `"checkins":{"rows_seen":0`, 1),
			want: "check-in page metadata",
		},
		{
			name: "checkin after evaluation",
			body: strings.Replace(canonical,
				`"sent_at":"2026-09-07T12:02:00Z","received_at":"2026-09-07T12:02:00Z"`,
				`"sent_at":"2026-09-07T12:02:00Z","received_at":"2026-09-07T12:03:00Z"`, 1),
			want: "after evaluated_at",
		},
		{
			name: "identity observed without clock",
			body: strings.Replace(canonical, `"identity":{"observed":false`, `"identity":{"observed":true`, 1),
			want: "identity observation metadata",
		},
		{
			name: "resource value without observation",
			body: strings.Replace(canonical,
				`"resources":{"observed":false,"decoded":false,"invalid":false,"observed_at":null,"value":null}`,
				`"resources":{"observed":false,"decoded":true,"invalid":false,"observed_at":null,"value":{"disk_free_bytes":2,"disk_total_bytes":1,"mem_total_bytes":1,"mem_available_bytes":1,"cpu_count":1,"load_1m":0,"mem_measured":true}}`, 1),
			want: "resources observation metadata",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, test.body, nil)
			defer server.Close()
			if result, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("result=%+v err=%v want=%q", result, err, test.want)
			}
		})
	}
}

func TestMachineReadClientValidatesTypedExpectationEvidence(t *testing.T) {
	makeResult := func() operator.MachineDetailResult {
		var result operator.MachineDetailResult
		if err := json.Unmarshal([]byte(validMachineDetailJSON(validMachineSummaryJSON)), &result); err != nil {
			t.Fatal(err)
		}
		text := func(value string, max int) operator.EvidenceText {
			return operator.EvidenceText{Text: value, MaxBytes: max, Bytes: len(value), Issues: []string{}}
		}
		observedAt := &operator.MachineEvidenceClock{
			MeasuredAt: time.Date(2026, 9, 7, 12, 2, 0, 0, time.UTC),
			ReceivedAt: time.Date(2026, 9, 7, 12, 2, 1, 0, time.UTC),
		}
		modifiedAt := time.Date(2026, 9, 7, 12, 1, 0, 0, time.UTC)
		lastAt := time.Date(2026, 9, 7, 12, 1, 30, 0, time.UTC)
		exists := true
		result.Expectations = operator.MachineExpectationSection{
			Configured: true,
			Rules: operator.MachineExpectationPage{Total: 1, Items: []operator.MachineExpectation{{
				Unit: text("proof.service", 256), Artifact: text("/tmp/proof.jsonl", 4096),
				Why: text("proof", 4096), MaxAgeSeconds: 3600,
				Observation: operator.MachineArtifactObservation{
					Observed: true, Decoded: true, ObservedAt: observedAt,
					Exists: &exists, ModifiedAt: &modifiedAt,
				},
				Events: &operator.MachineExpectationEvents{
					WindowSeconds: 1800,
					FailureTypes: operator.MachineEventTypePage{Total: 1,
						Items: []operator.EvidenceText{text("bad", 256)}},
					Observed: true, Decoded: true, ObservedAt: observedAt,
					Declared: operator.MachineEventCountPage{Total: 1, Items: []operator.MachineEventCount{{
						Type: text("bad", 256), Count: 1, LastAt: &lastAt,
					}}},
					Undeclared: operator.MachineEventCountPage{Items: []operator.MachineEventCount{}},
				},
			}}},
		}
		return result
	}
	if err := validateMachineDetailResult(makeResult(), "machine-1"); err != nil {
		t.Fatalf("canonical expectation evidence rejected: %v", err)
	}
	tests := []struct {
		name string
		edit func(*operator.MachineDetailResult)
		want string
	}{
		{"artifact read failure retains exists", func(v *operator.MachineDetailResult) {
			obs := &v.Expectations.Rules.Items[0].Observation
			obs.ReadFailed = true
			obs.Error = &operator.EvidenceText{Text: "denied", MaxBytes: 4096, Bytes: 6, Issues: []string{}}
		}, "decoded artifact observation metadata"},
		{"event page total mismatch", func(v *operator.MachineDetailResult) {
			v.Expectations.Rules.Items[0].Events.Declared.Total = 2
		}, "event count page metadata"},
		{"event timestamp beyond skew", func(v *operator.MachineDetailResult) {
			future := v.EvaluatedAt.Add(state.ClockSkewTolerance + time.Second)
			v.Expectations.Rules.Items[0].Events.Declared.Items[0].LastAt = &future
		}, "after evaluated_at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := makeResult()
			test.edit(&result)
			if err := validateMachineDetailResult(result, "machine-1"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
		})
	}
}

func TestMachineReadClientAcceptsCanonicalEmptyFleet(t *testing.T) {
	server := machineReadResponseServer(t, http.StatusOK, machineListPageJSON(nil, 0, 0, 0, nil), nil)
	defer server.Close()
	result, err := operatorClientForServer(t, server).Machines(t.Context())
	if err != nil || result.CreationCeiling != 0 || result.Total != 0 || result.MatchedTotal != 0 ||
		result.Items == nil || len(result.Items) != 0 || result.NextCursor != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestMachineReadClientEncodesCanonicalFilters(t *testing.T) {
	request := operator.MachineListRequest{
		MachineID: "machine-1", DisplayName: "cnode",
		States:    []state.State{state.Degraded, state.Online},
		Lifecycle: operator.MachineLifecycleActive,
		Reporting: operator.MachineReportingTrue, Channel: operator.MachineChannelNone, Limit: 7,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/operator/machines" {
			t.Errorf("method=%s path=%s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("machine_id") != "machine-1" || query.Get("display_name") != "cnode" ||
			query.Get("lifecycle") != "active" || query.Has("expected") ||
			query.Get("reporting") != "true" || query.Get("channel") != "none" ||
			query.Get("limit") != "7" || strings.Join(query["state"], ",") != "Online,Degraded" {
			t.Errorf("query=%v", query)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(validMachineListJSON(validMachineSummaryJSON)))
	}))
	defer server.Close()

	result, err := operatorClientForServer(t, server).ListMachines(t.Context(), request)
	if err != nil || result.SchemaVersion != operator.MachineListReadSchemaVersion ||
		result.Consistency != operator.MachineReadConsistency || result.CreationCeiling != 1 ||
		result.MatchedTotal != 1 || len(result.Items) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestMachineReadClientPagesWithFilterBoundCursor(t *testing.T) {
	request := operator.MachineListRequest{Limit: 1}
	firstAt := time.Date(2026, 9, 7, 12, 1, 0, 0, time.UTC)
	firstItem := machineSummaryJSON("machine-2", "tower", firstAt.Format(time.RFC3339))
	secondItem := validMachineSummaryJSON
	cursor := machineClientTestCursor(t, request, 2, firstAt, "machine-2")
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("limit") != "1" {
			t.Errorf("query=%v", r.URL.Query())
		}
		switch got := r.URL.Query().Get("cursor"); got {
		case "":
			_, _ = w.Write([]byte(machineListPageJSON([]string{firstItem}, 2, 2, 2, &cursor)))
		case cursor:
			_, _ = w.Write([]byte(machineListPageJSON([]string{secondItem}, 2, 2, 2, nil)))
		default:
			t.Errorf("unexpected cursor %q", got)
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)

	first, err := client.ListMachines(t.Context(), request)
	if err != nil || first.NextCursor == nil || *first.NextCursor != cursor ||
		len(first.Items) != 1 || first.Items[0].MachineID != "machine-2" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	request.Cursor = *first.NextCursor
	second, err := client.ListMachines(t.Context(), request)
	if err != nil || second.NextCursor != nil || second.CreationCeiling != first.CreationCeiling ||
		len(second.Items) != 1 || second.Items[0].MachineID != "machine-1" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("requests=%d", hits.Load())
	}
}

func TestMachineReadClientAcceptsServerCursorForNoStateFilter(t *testing.T) {
	serverNormalized := operator.MachineListRequest{
		States: []state.State{}, Lifecycle: operator.MachineLifecycleAny,
		Reporting: operator.MachineReportingAny,
		Channel:   operator.MachineChannelAny, Limit: operator.DefaultMachineReadLimit,
	}
	anchorAt := time.Date(2026, 9, 7, 12, 1, 0, 0, time.UTC)
	cursor := machineClientEncodedCursor(t, machineClientCursor{
		Version: operator.MachineListReadSchemaVersion, FilterDigest: machineClientFilterDigest(serverNormalized),
		CreationCeiling: 2,
		After:           operator.MachineReadPosition{CreatedAt: anchorAt, MachineID: "machine-2"},
	})
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("cursor") != cursor {
			t.Errorf("query=%v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(machineListPageJSON([]string{validMachineSummaryJSON}, 2, 2, 2, nil)))
	}))
	defer server.Close()

	result, err := operatorClientForServer(t, server).ListMachines(t.Context(), operator.MachineListRequest{Cursor: cursor})
	if err != nil || len(result.Items) != 1 || result.Items[0].MachineID != "machine-1" || hits.Load() != 1 {
		t.Fatalf("result=%+v requests=%d err=%v", result, hits.Load(), err)
	}
}

func TestMachineReadClientRejectsInvalidListRequestsBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	defaultCursor := machineClientTestCursor(t, operator.MachineListRequest{}, 1,
		time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), "machine-1")

	requests := []operator.MachineListRequest{
		{MachineID: " machine-1"},
		{DisplayName: "cnode\n"},
		{States: []state.State{state.Online, state.Online}},
		{States: []state.State{"Bogus"}},
		{Lifecycle: "deleted"},
		{Reporting: "stale"},
		{Channel: "beta"},
		{Limit: -1},
		{Limit: operator.MaxMachineReadLimit + 1},
		{Cursor: "not-a-canonical-cursor"},
		{Channel: operator.MachineChannelStable, Cursor: defaultCursor},
	}
	for _, request := range requests {
		if result, err := client.ListMachines(t.Context(), request); err == nil {
			t.Errorf("accepted request %+v: %+v", request, result)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached HTTP server %d times", hits.Load())
	}
}

func TestMachineReadClientRejectsListEnvelopeContradictions(t *testing.T) {
	request := operator.MachineListRequest{Limit: 1}
	firstAt := time.Date(2026, 9, 7, 12, 1, 0, 0, time.UTC)
	firstItem := machineSummaryJSON("machine-2", "tower", firstAt.Format(time.RFC3339))
	secondItem := validMachineSummaryJSON
	cursor := machineClientTestCursor(t, request, 2, firstAt, "machine-2")
	wrongAnchor := machineClientTestCursor(t, request, 2,
		time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), "machine-1")
	retiredItem := strings.NewReplacer(
		`"retired_at":null`, `"retired_at":"2026-09-07T12:02:00Z"`,
		`"state":"Online"`, `"state":null`,
		`"reporting":true`, `"reporting":null`,
		`"last_checkin_received_at":"2026-09-07T12:02:00Z"`, `"last_checkin_received_at":null`,
	).Replace(validMachineSummaryJSON)
	stateLowerBound := strings.ReplaceAll(validMachineListJSON(validMachineSummaryJSON),
		`{"state":"Online","count":1},{"state":"Degraded","count":0}`,
		`{"state":"Online","count":0},{"state":"Degraded","count":1}`)
	denominatorMismatch := strings.Replace(validMachineListJSON(validMachineSummaryJSON),
		`"denominator_state_counts":[
    {"state":"Online","count":1},{"state":"Degraded","count":0}`,
		`"denominator_state_counts":[
    {"state":"Online","count":0},{"state":"Degraded","count":1}`, 1)

	tests := []struct {
		name    string
		request operator.MachineListRequest
		body    string
		want    string
	}{
		{
			name: "unsupported consistency", request: operator.MachineListRequest{},
			body: strings.Replace(validMachineListJSON(validMachineSummaryJSON),
				`"registry_creation_ceiling_with_live_health"`, `"snapshot"`, 1),
			want: "unsupported machine read",
		},
		{
			name: "matched total exceeds fleet total", request: operator.MachineListRequest{},
			body: strings.Replace(validMachineListJSON(validMachineSummaryJSON),
				`"matched_total":1`, `"matched_total":2`, 1),
			want: "inconsistent machine list totals",
		},
		// 長度、加總與每格非負都正確，只有標籤不規範：Degraded 格消失且 Unreachable 重複，
		// 使機隊摘要整個漏掉一種狀態；釘住 machines.go:1105。
		// ⚠ 實測將 validateMachineStateCounts 整支焊成 return nil：本 case 紅；
		// 另有 state_buckets_do_not_match_active_envelope 紅，它守的是加總。
		// ⚠ 實測關掉 counts[i].State != candidate：本 case 紅、others=[]；
		// 全樹只有這一個 case 守著標籤規範。
		// ⚠ 對照組關掉 sum != total：本 case 仍綠，只有守加總的那一支紅。
		// ⚠ 對照組關掉 len(counts) != len(state.AllStates)：全綠；
		// 「不窮盡」那一條到現在仍然沒人守，刻意沒補。
		{
			name: "state counts carry a label outside the canonical order", request: operator.MachineListRequest{},
			body: func() string {
				body := validMachineListJSON(validMachineSummaryJSON)
				anchor := `{"state":"Online","count":1},{"state":"Degraded","count":0}`
				if count := strings.Count(body, anchor); count != 2 {
					t.Fatalf("state count label anchor occurs %d times, want 2", count)
				}
				return strings.Replace(body, anchor,
					`{"state":"Online","count":1},{"state":"Unreachable","count":0}`, 1)
			}(),
			want: "state counts are not canonical",
		},
		{
			name: "nonempty fleet has zero creation ceiling", request: operator.MachineListRequest{},
			body: strings.Replace(validMachineListJSON(validMachineSummaryJSON),
				`"creation_ceiling":1`, `"creation_ceiling":0`, 1),
			want: "creation ceiling",
		},
		{
			name: "page state exceeds matching envelope bucket", request: operator.MachineListRequest{},
			body: stateLowerBound,
			want: "page state counts exceed",
		},
		{
			name: "denominator states differ from active states", request: operator.MachineListRequest{},
			body: denominatorMismatch,
			want: "denominator state counts differ",
		},
		{
			name: "page retired count exceeds envelope", request: operator.MachineListRequest{},
			body: validMachineListJSON(retiredItem),
			want: "page totals exceed",
		},
		{
			name: "page reporting count exceeds envelope", request: operator.MachineListRequest{},
			body: strings.Replace(validMachineListJSON(validMachineSummaryJSON),
				`"reporting":1,`, `"reporting":0,`, 1),
			want: "page totals exceed",
		},
		{
			name: "unfiltered matched total differs from fleet total", request: operator.MachineListRequest{},
			body: machineListPageJSON([]string{firstItem}, 2, 2, 1, nil),
			want: "inconsistent matched_total",
		},
		{
			name: "active and denominator totals differ", request: operator.MachineListRequest{},
			body: strings.Replace(validMachineListJSON(validMachineSummaryJSON),
				`"expected":1,`, `"expected":0,`, 1),
			want: "inconsistent machine list totals",
		},
		{
			name: "first page silently truncates matches", request: request,
			body: machineListPageJSON([]string{firstItem}, 2, 2, 2, nil),
			want: "continuation",
		},
		{
			name: "complete first page invents continuation", request: request,
			body: machineListPageJSON([]string{firstItem}, 2, 1, 1, &cursor),
			want: "continuation",
		},
		{
			name: "next cursor does not anchor final item", request: request,
			body: machineListPageJSON([]string{firstItem}, 2, 2, 2, &wrongAnchor),
			want: "anchor",
		},
		{
			name: "page is not in keyset order", request: operator.MachineListRequest{Limit: 2},
			body: machineListPageJSON([]string{secondItem, firstItem}, 2, 2, 2, nil),
			want: "keyset order",
		},
		{
			name: "item contradicts exact filter", request: operator.MachineListRequest{MachineID: "other"},
			body: validMachineListJSON(validMachineSummaryJSON),
			want: "contradicts request filters",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, test.body, nil)
			defer server.Close()
			if result, err := operatorClientForServer(t, server).ListMachines(t.Context(), test.request); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("result=%+v err=%v want=%q body=%s", result, err, test.want, test.body)
			}
		})
	}
}

func TestMachineReadClientRejectsAmbiguousOrIncompleteBodies(t *testing.T) {
	duplicate := strings.Replace(validMachineSummaryJSON,
		`"machine_id":"machine-1"`, `"machine_id":"machine-1","machine_id":"machine-1"`, 1)
	unknown := strings.Replace(validMachineSummaryJSON,
		`"display_name":"cnode"`, `"display_name":"cnode","hostname":"private"`, 1)
	missing := strings.Replace(validMachineSummaryJSON, `  "channel":null,`+"\n", "", 1)
	nullExpected := strings.Replace(validMachineSummaryJSON, `"expected":true`, `"expected":null`, 1)
	tests := []struct {
		name string
		body string
		want string
	}{
		{"nested duplicate", validMachineListJSON(duplicate), "duplicate object field"},
		{"nested unknown", validMachineListJSON(unknown), "unknown field"},
		{"nested missing", validMachineListJSON(missing), "missing field"},
		{"null required scalar", validMachineListJSON(nullExpected), "null is not allowed"},
		{"null list", strings.Replace(validMachineListJSON(validMachineSummaryJSON), `"items":[`+validMachineSummaryJSON+`]`, `"items":null`, 1), "null is not allowed"},
		{"trailing", validMachineListJSON(validMachineSummaryJSON) + `{}`, "trailing JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, tt.body, nil)
			defer server.Close()
			_, err := operatorClientForServer(t, server).Machines(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q", err, tt.want)
			}
		})
	}
}

func TestMachineReadClientValidatesCanonicalProjectionIssues(t *testing.T) {
	canonical := strings.NewReplacer(
		`"issues":[]`, `"issues":["display_name_invalid_utf8","display_name_control_or_format_replaced","display_name_truncated"]`,
		`"altered_fields":[]`, `"altered_fields":["display_name"]`,
	).Replace(validMachineSummaryJSON)
	server := machineReadResponseServer(t, http.StatusOK, validMachineDetailJSON(canonical), nil)
	if _, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err != nil {
		server.Close()
		t.Fatalf("canonical projection issues rejected: %v", err)
	}
	server.Close()
	unnamed := strings.NewReplacer(
		`"display_name":"cnode"`, `"display_name":"(unnamed machine)"`,
		`"issues":[]`, `"issues":["display_name_missing"]`,
		`"altered_fields":[]`, `"altered_fields":["display_name"]`,
	).Replace(validMachineSummaryJSON)
	server = machineReadResponseServer(t, http.StatusOK, validMachineDetailJSON(unnamed), nil)
	if result, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err != nil ||
		result.Item.DisplayName != "(unnamed machine)" {
		server.Close()
		t.Fatalf("safe unnamed projection rejected: result=%+v err=%v", result, err)
	}
	server.Close()

	tests := []struct {
		name   string
		issues string
		want   string
	}{
		{
			name:   "duplicate",
			issues: `"issues":["display_name_invalid_utf8","display_name_invalid_utf8"]`,
			want:   "duplicated or not canonical",
		},
		{
			name:   "out of order",
			issues: `"issues":["display_name_truncated","display_name_control_or_format_replaced"]`,
			want:   "duplicated or not canonical",
		},
		{
			name: "too many",
			issues: `"issues":["display_name_invalid_utf8","display_name_control_or_format_replaced",` +
				`"display_name_missing","display_name_truncated"]`,
			want: "issue evidence is inconsistent",
		},
		{
			name:   "unknown",
			issues: `"issues":["display_name_redacted"]`,
			want:   "issue is unknown",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := strings.NewReplacer(
				`"issues":[]`, test.issues,
				`"altered_fields":[]`, `"altered_fields":["display_name"]`,
			).Replace(validMachineSummaryJSON)
			server := machineReadResponseServer(t, http.StatusOK, validMachineDetailJSON(item), nil)
			defer server.Close()
			if result, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("result=%+v err=%v want=%q", result, err, test.want)
			}
		})
	}

	nullIssues := strings.Replace(validMachineSummaryJSON, `"issues":[]`, `"issues":null`, 1)
	server = machineReadResponseServer(t, http.StatusOK, validMachineDetailJSON(nullIssues), nil)
	defer server.Close()
	if result, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err == nil ||
		!strings.Contains(err.Error(), "null is not allowed") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestMachineReadClientAcceptsSeverityFourAndRejectsContradictoryFacts(t *testing.T) {
	severityFour := strings.Replace(validMachineDetailJSON(validMachineSummaryJSON),
		`"findings":{"total":0,"invalid":0,"truncated":false,"items":[]}`,
		`"findings":{"total":1,"invalid":0,"truncated":false,"items":[{"kind":"agent","severity":4,"advisory":false,"message":{"text":"crash loop","max_bytes":4096,"bytes":10,"truncated":false,"issues":[]}}]}`, 1)
	server := machineReadResponseServer(t, http.StatusOK, severityFour, nil)
	if _, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err != nil {
		server.Close()
		t.Fatalf("canonical severity-four finding rejected: %v", err)
	}
	server.Close()

	tests := []struct {
		name   string
		body   string
		detail bool
		want   string
	}{
		{
			name: "state buckets do not match active envelope",
			body: strings.Replace(validMachineListJSON(validMachineSummaryJSON),
				`{"state":"Online","count":1}`, `{"state":"Online","count":0}`, 1),
			want: "visible state counts total",
		},
		{
			name:   "online without reporting",
			body:   strings.Replace(validMachineDetailJSON(validMachineSummaryJSON), `"reporting":true`, `"reporting":false`, 1),
			detail: true,
			want:   "reporting state lacks",
		},
		{
			name: "never reported with checkin",
			body: strings.Replace(strings.Replace(
				validMachineDetailJSON(validMachineSummaryJSON), `"state":"Online"`, `"state":"NeverReported"`, 1),
				`"reporting":true`, `"reporting":false`, 1),
			detail: true,
			want:   "never-reported state has contradictory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, tt.body, nil)
			defer server.Close()
			client := operatorClientForServer(t, server)
			var err error
			if tt.detail {
				_, err = client.Machine(t.Context(), "machine-1")
			} else {
				_, err = client.Machines(t.Context())
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q body=%s", err, tt.want, tt.body)
			}
		})
	}
}

func TestMachineReadClientRejectsHealthEvidenceAfterEvaluation(t *testing.T) {
	future := "2026-09-07T12:02:31Z"
	tests := []struct {
		name   string
		body   string
		detail bool
	}{
		{
			name: "list state_since",
			body: validMachineListJSON(strings.Replace(validMachineSummaryJSON,
				`"state_since":null`, `"state_since":"`+future+`"`, 1)),
		},
		{
			name: "detail last checkin",
			body: validMachineDetailJSON(strings.Replace(validMachineSummaryJSON,
				`"last_checkin_received_at":"2026-09-07T12:02:00Z"`,
				`"last_checkin_received_at":"`+future+`"`, 1)),
			detail: true,
		},
		{
			name: "detail last observation",
			body: validMachineDetailJSON(strings.Replace(validMachineSummaryJSON,
				`"last_observation_received_at":null`,
				`"last_observation_received_at":"`+future+`"`, 1)),
			detail: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, test.body, nil)
			defer server.Close()
			client := operatorClientForServer(t, server)
			var err error
			if test.detail {
				_, err = client.Machine(t.Context(), "machine-1")
			} else {
				_, err = client.Machines(t.Context())
			}
			if err == nil || !strings.Contains(err.Error(), "after evaluated_at") {
				t.Fatalf("error=%v body=%s", err, test.body)
			}
		})
	}
}

func TestMachineReadClientKeepsRetiredJudgementOutsideFleetState(t *testing.T) {
	retired := strings.Replace(validMachineSummaryJSON, `"retired_at":null`, `"retired_at":"2026-09-07T12:02:00Z"`, 1)
	retired = strings.Replace(retired, `"state":"Online"`, `"state":null`, 1)
	retired = strings.Replace(retired, `"reporting":true`, `"reporting":null`, 1)
	retired = strings.Replace(retired, `"last_checkin_received_at":"2026-09-07T12:02:00Z"`, `"last_checkin_received_at":null`, 1)

	server := machineReadResponseServer(t, http.StatusOK, validMachineDetailJSON(retired), nil)
	if _, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err != nil {
		server.Close()
		t.Fatalf("canonical retired detail rejected: %v", err)
	}
	server.Close()

	withFinding := strings.Replace(validMachineDetailJSON(retired),
		`"findings":{"total":0,"invalid":0,"truncated":false,"items":[]}`,
		`"findings":{"total":1,"invalid":0,"truncated":false,"items":[{"kind":"agent","severity":1,"advisory":true,"message":{"text":"retained","max_bytes":4096,"bytes":8,"truncated":false,"issues":[]}}]}`, 1)
	server = machineReadResponseServer(t, http.StatusOK, withFinding, nil)
	if result, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1"); err != nil ||
		result.Judgement.AffectsFleetState || result.Judgement.Findings.Total != 1 {
		server.Close()
		t.Fatalf("retired retained judgement=%+v error=%v", result.Judgement, err)
	}
	server.Close()

	contradictory := strings.Replace(validMachineDetailJSON(retired),
		`"affects_fleet_state":false`, `"affects_fleet_state":true`, 1)
	server = machineReadResponseServer(t, http.StatusOK, contradictory, nil)
	defer server.Close()
	_, err := operatorClientForServer(t, server).Machine(t.Context(), "machine-1")
	if err == nil || !strings.Contains(err.Error(), "retired machine judgement") {
		t.Fatalf("retired fleet-state contradiction error=%v", err)
	}
}

func TestMachineReadClientRejectsHeaderStatusAndTargetMismatches(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers http.Header
		want    string
	}{
		{"missing no-store", http.StatusOK, http.Header{"Content-Type": {"application/json"}}, "no-store"},
		{"etag", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "ETag": {`"unsafe"`}}, "ETag"},
		{"unexpected status", http.StatusCreated, nil, "unexpected success status"},
		{"replay", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Idempotency-Replayed": {"true"}}, "idempotency replay"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := machineReadResponseServer(t, tt.status, validMachineListJSON(validMachineSummaryJSON), tt.headers)
			defer server.Close()
			_, err := operatorClientForServer(t, server).Machines(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q", err, tt.want)
			}
		})
	}

	server := machineReadResponseServer(t, http.StatusOK,
		validMachineDetailJSON(validMachineSummaryJSON), nil)
	defer server.Close()
	_, err := operatorClientForServer(t, server).Machine(t.Context(), "different-machine")
	if err == nil || !strings.Contains(err.Error(), "does not match request") {
		t.Fatalf("target mismatch error=%v", err)
	}
}

func TestMachineReadClientEnforcesResponseSizeCap(t *testing.T) {
	server := machineReadResponseServer(t, http.StatusOK, strings.Repeat(" ", maxResponseBytes+1), nil)
	defer server.Close()
	_, err := operatorClientForServer(t, server).Machines(t.Context())
	if err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversized response error=%v", err)
	}
	detailServer := machineReadResponseServer(t, http.StatusOK,
		strings.Repeat(" ", int(maxMachineDetailResponseBytes)+1), nil)
	defer detailServer.Close()
	_, err = operatorClientForServer(t, detailServer).Machine(t.Context(), "machine-1")
	if err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
		t.Fatalf("oversized detail response error=%v", err)
	}
}

func machineReadResponseServer(t *testing.T, status int, body string, headers http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if headers == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
		} else {
			for name, values := range headers {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestResourcesCannotCarryMemoryBytesWithoutHavingMeasuredThem(t *testing.T) {
	evaluatedAt := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	clock := &operator.MachineEvidenceClock{MeasuredAt: evaluatedAt, ReceivedAt: evaluatedAt}
	makeResult := func(resources operator.MachineResources) operator.MachineDetailResult {
		return operator.MachineDetailResult{
			EvaluatedAt: evaluatedAt,
			Resources: operator.MachineResourceEvidence{
				Observed: true, Decoded: true, ObservedAt: clock, Value: &resources,
			},
		}
	}

	withBytes := makeResult(operator.MachineResources{MemTotalBytes: 16 << 30})
	if err := validateMachineResources(withBytes); err == nil {
		t.Fatal("未量到記憶體卻仍攜帶 bytes 必須被拒絕；否則 client 會接受矛盾資料，網頁把沒量到畫成 0 B，operator 會誤以為機器記憶體耗盡")
	}
	withoutBytes := makeResult(operator.MachineResources{})
	if err := validateMachineResources(withoutBytes); err != nil {
		t.Fatalf("未量到記憶體且兩個數字都是 0 應可通過，實際錯誤為 %v；否則正確的未知值無法呈現，operator 仍可能把機器誤判成記憶體耗盡", err)
	}
}

func TestResourcesAcceptAnUnmeasuredLoadButStillRejectABadOne(t *testing.T) {
	evaluatedAt := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	clock := &operator.MachineEvidenceClock{MeasuredAt: evaluatedAt, ReceivedAt: evaluatedAt}
	makeResult := func(load *float64) operator.MachineDetailResult {
		return operator.MachineDetailResult{
			EvaluatedAt: evaluatedAt,
			Resources: operator.MachineResourceEvidence{
				Observed: true, Decoded: true, ObservedAt: clock,
				Value: &operator.MachineResources{Load1m: load},
			},
		}
	}

	if err := validateMachineResources(makeResult(nil)); err != nil {
		t.Fatalf("沒量到的負載應可通過驗證，實際錯誤為 %v；否則 client 無法呈現未知，operator 可能以為那台 Mac 很閒", err)
	}
	negative := -1.0
	if err := validateMachineResources(makeResult(&negative)); err == nil {
		t.Fatal("負負載必須被拒絕；否則 client 會接受不可能的量測值")
	}
	nan := math.NaN()
	if err := validateMachineResources(makeResult(&nan)); err == nil {
		t.Fatal("NaN 負載必須被拒絕；否則 client 會接受無法顯示的量測值")
	}
}
