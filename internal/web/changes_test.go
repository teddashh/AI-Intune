package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

var changeNextLinkRE = regexp.MustCompile("<a rel=\"next\" href=\"([^\"]+)\">下一頁")
var changeFirstLinkRE = regexp.MustCompile("<a href=\"([^\"]+)\">回到這次查詢的第一頁</a>")

func changeLink(body string, expression *regexp.Regexp) string {
	match := expression.FindStringSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return html.UnescapeString(match[1])
}

func TestChangesPageRejectsUnknownAmbiguousAndNonCanonicalQuery(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{
		"/reports/changes?",
		"/reports/changes?unknown=value",
		"/reports/changes?machine_id=one&machine_id=two",
		"/reports/changes?subject=one&subject=two",
		"/reports/changes?from=2026-09-08T12%3A00%3A00Z&from=2026-09-08T12%3A00%3A00Z",
		"/reports/changes?to=2026-09-08T12%3A00%3A00Z&to=2026-09-08T12%3A00%3A00Z",
		"/reports/changes?limit=1&limit=2",
		"/reports/changes?cursor=one&cursor=two",
		"/reports/changes?machine_id=%20",
		"/reports/changes?subject=%E2%80%AE",
		"/reports/changes?kind=",
		"/reports/changes?kind=state&kind=state",
		"/reports/changes?kind=not-a-kind",
		"/reports/changes?from=yesterday",
		"/reports/changes?from=2026-09-08T12%3A00%3A00.000Z",
		"/reports/changes?from=2026-09-08T12%3A00%3A00.1Z",
		"/reports/changes?from=2026-09-09T12%3A00%3A00Z&to=2026-09-08T12%3A00%3A00Z",
		"/reports/changes?from=2026-01-01T00%3A00%3A00Z&to=2026-09-08T00%3A00%3A00Z",
		"/reports/changes?to=2999-01-01T00%3A00%3A00Z",
		"/reports/changes?limit=",
		"/reports/changes?limit=0",
		"/reports/changes?limit=101",
		"/reports/changes?limit=01",
		"/reports/changes?limit=%2B1",
		"/reports/changes?limit=1.0",
		"/reports/changes?cursor=",
		"/reports/changes?cursor=not-a-cursor",
	} {
		t.Run(path, func(t *testing.T) {
			response := doGet(t, s, path)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), "Changes filter 或 cursor 不合法") {
				t.Fatalf("GET %s = %d: %s", path, response.Code, response.Body.String())
			}
		})
	}
}

func TestChangesPageAcceptsNativeFormEmptyOptionalFieldsAndOffsetTimes(t *testing.T) {
	s, _ := newServer(t)
	query := url.Values{
		"machine_id": {""}, "subject": {""}, "from": {""}, "to": {""}, "limit": {"50"},
	}.Encode()
	response := doGet(t, s, "/reports/changes?"+query)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "</html>") {
		t.Fatalf("native Changes form submission = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Changes cache policy=%q, want no-store", response.Header().Get("Cache-Control"))
	}

	request := httptest.NewRequest(http.MethodGet,
		"/reports/changes?from=2026-09-08T08%3A00%3A00-04%3A00&to=2026-09-08T09%3A00%3A00-04%3A00", nil)
	parsed, err := parseChangePageRequest(request)
	if err != nil || parsed.From == nil || parsed.To == nil ||
		parsed.From.Format(time.RFC3339) != "2026-09-08T12:00:00Z" ||
		parsed.To.Format(time.RFC3339) != "2026-09-08T13:00:00Z" {
		t.Fatalf("canonical offset window parsed as %+v, err=%v", parsed, err)
	}
}

func TestChangesPageFiltersAndPagesTheSharedReadModel(t *testing.T) {
	s, st := newServer(t)
	ids := make([]string, 0, 3)
	for _, name := range []string{"changes-a", "changes-b", "changes-c"} {
		id, _, err := st.CreateEnrollTokenFor(name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	filtered := get(t, s, "/reports/changes?machine_id="+url.QueryEscape(ids[1])+
		"&kind=registry&subject=lifecycle&limit=10")
	if got := strings.Count(filtered, "data-change-kind=\"registry\""); got != 1 {
		t.Fatalf("filtered Changes rows=%d, want one", got)
	}
	for _, want := range []string{
		"href=\"/machines/" + ids[1] + "\"",
		"name=\"kind\" value=\"registry\" checked",
		"name=\"subject\" value=\"lifecycle\"",
		"Hub changed_at",
		"Agent measured_at",
		"Before",
		"After",
		"Kind counts",
		"Creation ceilings",
	} {
		if !strings.Contains(filtered, want) {
			t.Errorf("filtered Changes page missing %q", want)
		}
	}
	for i, id := range ids {
		if i != 1 && strings.Contains(filtered, "href=\"/machines/"+id+"\"") {
			t.Errorf("filtered Changes page leaked non-matching machine %s", id)
		}
	}

	body := get(t, s, "/reports/changes?kind=registry&subject=lifecycle&limit=1")
	seen := make(map[string]bool, len(ids))
	for pageNumber := 1; ; pageNumber++ {
		found := ""
		for _, id := range ids {
			if strings.Contains(body, "href=\"/machines/"+id+"\"") {
				if found != "" {
					t.Fatalf("page %d contains more than one matching row", pageNumber)
				}
				found = id
			}
		}
		if found == "" || seen[found] {
			t.Fatalf("page %d machine=%q already seen=%v", pageNumber, found, seen)
		}
		seen[found] = true
		if !strings.Contains(body, "精確篩選後 3 筆；本頁 1 筆") {
			t.Fatalf("page %d did not preserve matched total and page size", pageNumber)
		}

		next := changeLink(body, changeNextLinkRE)
		if next == "" {
			break
		}
		parsed, err := url.Parse(next)
		if err != nil || parsed.Path != "/reports/changes" ||
			strings.Join(parsed.Query()["kind"], ",") != "registry" ||
			parsed.Query().Get("subject") != "lifecycle" || parsed.Query().Get("limit") != "1" ||
			parsed.Query().Get("from") == "" || parsed.Query().Get("to") == "" ||
			parsed.Query().Get("cursor") == "" {
			t.Fatalf("page %d next href did not freeze filters/window: href=%q err=%v", pageNumber, next, err)
		}
		body = get(t, s, parsed.RequestURI())
		first := changeLink(body, changeFirstLinkRE)
		firstURL, err := url.Parse(first)
		if first == "" || err != nil || firstURL.Query().Get("cursor") != "" ||
			firstURL.Query().Get("from") == "" || firstURL.Query().Get("to") == "" {
			t.Fatalf("page %d first-page recovery link=%q err=%v", pageNumber+1, first, err)
		}
		if pageNumber > len(ids) {
			t.Fatal("Changes pagination did not terminate")
		}
	}
	if len(seen) != len(ids) {
		t.Fatalf("paged traversal saw %d machines, want %d: %v", len(seen), len(ids), seen)
	}
}

func TestChangesPageRendersOnlyTypedEvidenceAndSeparatesHubAndAgentTimes(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "safe-change")
	const secret = "RAW_CHANGE_PAYLOAD_SECRET_7f2a"
	measuredAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	receivedAt := measuredAt.Add(20 * time.Second)
	observation := batch(measuredAt)
	observation.Identity.Hostname = secret
	observation.Identity.UnixUser = secret
	observation.Identity.OS = "linux-safe"
	observation.Identity.Kernel = "6.8-safe"
	if err := st.RecordObservation(id, observation, receivedAt); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRow(
		"SELECT payload FROM observed_state WHERE machine_id=? AND kind=? ORDER BY rowid DESC LIMIT 1",
		id, store.KindIdentity).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, secret) {
		t.Fatal("sensitive fixture is not present in the raw Store payload")
	}

	body := get(t, s, "/reports/changes?machine_id="+url.QueryEscape(id)+
		"&kind="+operator.ChangeKindIdentity+"&subject=identity&limit=10")
	if strings.Contains(body, secret) {
		t.Fatalf("Changes HTML leaked raw observation payload marker %q", secret)
	}
	for _, want := range []string{
		"data-change-kind=\"identity\"",
		"OS",
		"linux-safe",
		"kernel",
		"6.8-safe",
		"Hub changed_at",
		"Agent measured_at",
		"省略欄位",
		"host_identity",
		"window 起訖點比較",
		"href=\"/machines/" + id + "\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("typed Changes evidence missing %q", want)
		}
	}
	if strings.Count(body, "Hub changed_at") != 1 || strings.Count(body, "Agent measured_at") != 1 {
		t.Error("Hub changed_at and agent measured_at were not rendered as distinct coordinates")
	}
}

// TestChangesPageRendersUnmeasuredSystemdEvidence 釘住未量到是明確證據，而不是不安全資料或 Reason 原文。
func TestChangesPageRendersUnmeasuredSystemdEvidence(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "systemd-unmeasured-change")
	now := time.Now().UTC().Truncate(time.Second)
	before := batch(now.Add(-2 * time.Minute))
	before.Systemd = []model.Unit{{Name: "clawctl-agent.service", Present: true, Measured: true, ActiveState: "active", SubState: "running", NRestarts: 3}}
	after := batch(now.Add(-time.Minute))
	after.Systemd = []model.Unit{{Name: "clawctl-agent.service", Reason: "Failed to connect to bus"}}
	if err := st.RecordObservation(id, before, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(id, after, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/reports/changes?machine_id="+url.QueryEscape(id)+"&kind="+operator.ChangeKindSystemd+"&subject=clawctl-agent.service&limit=10")
	if !strings.Contains(body, "沒有量到") {
		t.Fatalf("HTML 未呈現未量到證據：%s", body)
	}
	for _, forbidden := range []string{"此列沒有可安全呈現的型別值", "Failed to connect to bus"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("HTML 不應出現 %q：%s", forbidden, body)
		}
	}
}

func TestChangeCoveragePartialViewIsExplicit(t *testing.T) {
	partial, messages := changeCoverageMessages(operator.ChangeCoverage{
		ObservationComparison:    "endpoint_delta",
		ObservationHistory:       "partial",
		RegistryHistory:          "partial_before_tracking_started",
		StateHistory:             "partial_before_tracking_started",
		MalformedTimestampRows:   3,
		UnplaceableTimestampRows: 1,
		Issues:                   []string{"observation_history_pruned"},
	})
	if !partial {
		t.Fatal("partial coverage was presented as complete")
	}
	joined := strings.Join(messages, "\n")
	for _, want := range []string{"retention", "Registry lifecycle", "State transition", "3", "1", "observation_history_pruned"} {
		if !strings.Contains(joined, want) {
			t.Errorf("partial coverage explanation missing %q: %s", want, joined)
		}
	}
	complete, completeMessages := changeCoverageMessages(operator.ChangeCoverage{
		ObservationHistory: "complete", RegistryHistory: "complete", StateHistory: "complete",
	})
	if complete || len(completeMessages) != 0 {
		t.Fatalf("complete input coverage was presented as partial: %v %v", complete, completeMessages)
	}
	notApplicable, notApplicableMessages := changeCoverageMessages(operator.ChangeCoverage{
		ObservationHistory: operator.ChangeCoverageNotApplicable,
		RegistryHistory:    operator.ChangeCoverageNotApplicable,
		StateHistory:       operator.ChangeCoverageNotApplicable,
	})
	if notApplicable || len(notApplicableMessages) != 0 {
		t.Fatalf("non-applicable input coverage was presented as partial: %v %v", notApplicable, notApplicableMessages)
	}
}

func TestChangesPageRedactsUnknownCredentialSubjectFromHTMLLinksAndErrors(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "private-change-subject")
	const privateSubject = "alice@example.com"
	now := time.Now().UTC().Truncate(time.Second)
	observation := batch(now.Add(-time.Minute))
	observation.Credentials = append(observation.Credentials, model.Credential{
		Provider: privateSubject, Status: model.CredExpired,
	})
	if err := st.RecordObservation(id, observation, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/reports/changes?machine_id="+url.QueryEscape(id)+"&kind=credential&limit=1")
	if strings.Contains(body, privateSubject) || !strings.Contains(body, "(redacted subject)") ||
		!strings.Contains(body, "subject_not_allowlisted") {
		t.Fatalf("private subject projection was unsafe: %s", body)
	}
	if next := changeLink(body, changeNextLinkRE); strings.Contains(next, privateSubject) {
		t.Fatalf("next URL leaked private subject: %q", next)
	}

	rejected := doGet(t, s, "/reports/changes?subject="+url.QueryEscape(privateSubject))
	if rejected.Code != http.StatusBadRequest || strings.Contains(rejected.Body.String(), privateSubject) {
		t.Fatalf("status=%d rejected filter reflected private subject: %s", rejected.Code, rejected.Body.String())
	}
}

func TestChangesPageTypedErrorsHaveStableHTTPStatus(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		code  int
		retry bool
	}{
		{"invalid", operator.ErrInvalidChangeRead, http.StatusBadRequest, false},
		{"gone", operator.ErrChangeReadTraversalGone, http.StatusGone, false},
		{"broad", operator.ErrChangeReadTooBroad, http.StatusUnprocessableEntity, false},
		{"busy", operator.ErrChangeReadBusy, http.StatusTooManyRequests, true},
		{"timeout", operator.ErrChangeReadTimedOut, http.StatusServiceUnavailable, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if !writeChangePageReadError(recorder, test.err) || recorder.Code != test.code ||
				(recorder.Header().Get("Retry-After") != "") != test.retry {
				t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
}
