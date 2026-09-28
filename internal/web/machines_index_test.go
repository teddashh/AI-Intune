package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

var machineIndexRowLinkRE = regexp.MustCompile(`<a href="/machines/([^"]+)">([^<]*)</a>`)
var machineIndexNextLinkRE = regexp.MustCompile(`href="([^"]+)">下一頁`)

func machineIndexRowsHTML(t *testing.T, body string) string {
	t.Helper()
	section := strings.Index(body, `<h2 id="machines"`)
	if section < 0 {
		t.Fatal("machine index is missing its #machines heading")
	}
	body = body[section:]
	start := strings.Index(body, "<tbody>")
	end := strings.Index(body, "</tbody>")
	if start < 0 || end < start {
		t.Fatal("machine index table body is missing or truncated")
	}
	return body[start : end+len("</tbody>")]
}

func machineIndexRowIDs(t *testing.T, body string) []string {
	t.Helper()
	matches := machineIndexRowLinkRE.FindAllStringSubmatch(machineIndexRowsHTML(t, body), -1)
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		ids = append(ids, html.UnescapeString(match[1]))
	}
	return ids
}

func machineIndexNextHref(body string) string {
	match := machineIndexNextLinkRE.FindStringSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return html.UnescapeString(match[1])
}

func TestMachineIndexCanonicalFiltersReachTheSharedReadModel(t *testing.T) {
	s, st := newServer(t)
	canaryID := onlineMachine(t, st, "canary-online")
	observerID := onlineMachine(t, st, "observer-online")
	silentID := enroll(t, st, "stable-silent", time.Now().UTC())
	retiredID := onlineMachine(t, st, "stable-retired")
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, canaryID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET expected=0 WHERE machine_id=?`, observerID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='stable' WHERE machine_id IN (?,?)`, silentID, retiredID); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(retiredID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "exact identity",
			path: "/machines?display_name=canary-online&machine_id=" + url.QueryEscape(canaryID) + "&limit=10",
			want: canaryID,
		},
		{
			name: "active reporting canary",
			path: "/machines?channel=canary&lifecycle=active&reporting=true&limit=10",
			want: canaryID,
		},
		{
			name: "never reported stable",
			path: "/machines?channel=stable&lifecycle=active&reporting=false&state=NeverReported&limit=10",
			want: silentID,
		},
		{
			name: "unassigned reporting",
			path: "/machines?channel=none&lifecycle=active&reporting=true&limit=10",
			want: observerID,
		},
		{
			name: "retired unknown reporting",
			path: "/machines?channel=stable&lifecycle=retired&reporting=unknown&limit=10",
			want: retiredID,
		},
	}
	allIDs := []string{canaryID, observerID, silentID, retiredID}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := get(t, s, test.path)
			ids := machineIndexRowIDs(t, body)
			if len(ids) != 1 || ids[0] != test.want {
				t.Fatalf("GET %s machine rows=%v, want only %s", test.path, ids, test.want)
			}
			rows := machineIndexRowsHTML(t, body)
			for _, id := range allIDs {
				if id != test.want && strings.Contains(rows, id) {
					t.Errorf("GET %s leaked non-matching machine %s into the index", test.path, id)
				}
			}
			if !strings.Contains(body, "符合 filter 1 / registry ceiling 內 4 台") {
				t.Errorf("GET %s did not preserve full-fleet and matched totals", test.path)
			}
		})
	}
}

func TestMachineIndexPagingPreservesFiltersAndCreationCeiling(t *testing.T) {
	s, st := newServer(t)
	original := make(map[string]bool, 3)
	for _, name := range []string{"page-a", "page-b", "page-c"} {
		id, _, err := st.CreateEnrollTokenFor(name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		original[id] = true
	}

	body := get(t, s, "/machines?lifecycle=active&limit=1")
	firstIDs := machineIndexRowIDs(t, body)
	if len(firstIDs) != 1 {
		t.Fatalf("first page rows=%v, want one", firstIDs)
	}
	newID, _, err := st.CreateEnrollTokenFor("inserted-after-first-page", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{firstIDs[0]: true}
	for page := 1; ; page++ {
		if !strings.Contains(body, "符合 filter 3 / registry ceiling 內 3 台") {
			t.Fatalf("page %d changed the ceiling traversal totals", page)
		}
		next := machineIndexNextHref(body)
		if next == "" {
			break
		}
		u, err := url.Parse(next)
		if err != nil || u.Path != "/machines" || u.Fragment != "machines" ||
			u.Query().Has("expected") || u.Query().Get("lifecycle") != "active" ||
			u.Query().Get("limit") != "1" || u.Query().Get("cursor") == "" {
			t.Fatalf("page %d next href did not preserve canonical filters: href=%q err=%v", page, next, err)
		}
		body = get(t, s, u.RequestURI())
		ids := machineIndexRowIDs(t, body)
		if len(ids) != 1 || seen[ids[0]] {
			t.Fatalf("page %d rows=%v already-seen=%v", page+1, ids, seen)
		}
		seen[ids[0]] = true
		if !strings.Contains(body, ">回第一頁</a>") {
			t.Fatalf("continuation page %d does not expose a first-page recovery link", page+1)
		}
		if page > len(original) {
			t.Fatal("machine pagination did not terminate")
		}
	}
	if len(seen) != len(original) {
		t.Fatalf("paged traversal saw %d machines, want %d: %v", len(seen), len(original), seen)
	}
	for id := range original {
		if !seen[id] {
			t.Errorf("paged traversal omitted original machine %s", id)
		}
	}
	if seen[newID] {
		t.Fatalf("machine inserted above the first-page creation ceiling entered traversal: %s", newID)
	}
}

func TestMachineIndexRejectsUnknownAmbiguousAndNonCanonicalQuery(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{
		"/machines?",
		"/machines?unknown=value",
		"/machines?machine_id=one&machine_id=two",
		"/machines?machine_id=&machine_id=",
		"/machines?display_name=&display_name=",
		"/machines?machine_id=%20",
		"/machines?display_name=%E2%80%AE",
		"/machines?cursor=",
		"/machines?state=Online&state=Online",
		"/machines?state=not-a-state",
		"/machines?lifecycle=deleted",
		"/machines?expected=true",
		"/machines?reporting=yes",
		"/machines?channel=beta",
		"/machines?limit=0",
		"/machines?limit=101",
		"/machines?limit=01",
		"/machines?limit=%2B1",
		"/machines?limit=1.0",
		"/machines?cursor=not-a-cursor",
		"/machines?section=retired",
		"/machines?machines=attention&state=Degraded",
	} {
		t.Run(path, func(t *testing.T) {
			response := doGet(t, s, path)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), "machine filter 或 cursor 不合法") {
				t.Fatalf("GET %s = %d: %s", path, response.Code, response.Body.String())
			}
		})
	}
}

func TestMachineIndexAcceptsTheNativeFormEmptyOptionalTextFields(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "native-form")
	query := url.Values{
		"machine_id": {""}, "display_name": {""}, "lifecycle": {"any"},
		"reporting": {"any"}, "channel": {"any"},
		"limit": {"50"},
	}.Encode()
	response := doGet(t, s, "/machines?"+query)
	if response.Code != http.StatusOK {
		t.Fatalf("native GET form submission = %d: %s", response.Code, response.Body.String())
	}
	if ids := machineIndexRowIDs(t, response.Body.String()); len(ids) != 1 || ids[0] != id {
		t.Fatalf("native GET form submission rows=%v, want %s", ids, id)
	}
	if strings.Contains(response.Body.String(), `name="expected"`) {
		t.Fatal("machine index still renders the removed expected filter")
	}
}

func TestMachineIndexRendersSafeProjectionAndNoPrivateRegistryFields(t *testing.T) {
	s, st := newServer(t)
	id, _, err := st.CreateEnrollTokenFor("safe-before-corruption", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	unsafeName := "unsafe<script>\u202ename"
	if _, err := st.DB().Exec(`UPDATE machine_registry
 SET display_name=?, hostname='PRIVATE-HOST', unix_user='PRIVATE-USER', tailscale_ip='100.64.200.10', notes='PRIVATE-NOTE'
 WHERE machine_id=?`, unsafeName, id); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines?machine_id="+url.QueryEscape(id))
	rows := machineIndexRowsHTML(t, body)
	for _, want := range []string{id, `unsafe&lt;script&gt;�name`, "display_name_control_or_format_replaced"} {
		if !strings.Contains(rows, want) {
			t.Errorf("safe machine row is missing %q: %s", want, rows)
		}
	}
	for _, forbidden := range []string{unsafeName, "\u202e", "<script>", "PRIVATE-HOST", "PRIVATE-USER", "100.64.200.10", "PRIVATE-NOTE"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("machine index page disclosed %q", forbidden)
		}
	}
}

func TestMachineIndexAncillarySectionsUseTheSafeDisplayNameProjection(t *testing.T) {
	s, st := newServer(t)
	activeID := onlineMachine(t, st, "unsafe-active-before-corruption")
	credentialTargetID := onlineMachine(t, st, "unsafe-credential-target-before-corruption")
	neverID, _, err := st.CreateEnrollTokenFor("unsafe-never-before-corruption", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Make both agent runs long enough to overlap by more than five minutes.
	// Their default observation-age and clock-skew fields also provide enough
	// rows for the dead-signal section, while onlineMachine supplied tool data.
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 12; i++ {
		firstReceived := now.Add(-34*time.Minute + time.Duration(i)*3*time.Minute)
		secondReceived := firstReceived.Add(30 * time.Second)
		first := checkin(firstReceived)
		first.AgentStartedAt = now.Add(-2 * time.Hour)
		if err := st.RecordCheckin(activeID, first, firstReceived); err != nil {
			t.Fatalf("record first agent run check-in %d: %v", i, err)
		}
		second := checkin(secondReceived)
		second.AgentStartedAt = now.Add(-90 * time.Minute)
		if err := st.RecordCheckin(activeID, second, secondReceived); err != nil {
			t.Fatalf("record second agent run check-in %d: %v", i, err)
		}
	}
	jobID, token, jobNow := createWebJob(t, st, activeID, false)
	failWebJob(t, st, activeID, jobID, token, jobNow)
	recordCredential := func(machineID string, receivedAt, fileMTime, expiresAt time.Time, status model.CredStatus) {
		t.Helper()
		observation := batch(receivedAt)
		observation.Credentials = []model.Credential{{
			Provider: "claude", Status: status, FileMTime: &fileMTime, ExpiresAt: &expiresAt,
		}}
		if err := st.RecordObservation(machineID, observation, receivedAt); err != nil {
			t.Fatalf("record credential observation for %s: %v", machineID, err)
		}
	}
	peerOldMTime := now.Add(-10 * time.Hour)
	peerNewMTime := now.Add(-time.Hour)
	recordCredential(activeID, now.Add(-2*time.Hour), peerOldMTime, peerOldMTime.Add(8*time.Hour), model.CredConfigured)
	recordCredential(activeID, now.Add(-30*time.Minute), peerNewMTime, peerNewMTime.Add(8*time.Hour), model.CredConfigured)
	targetMTime := now.Add(-20 * time.Hour)
	recordCredential(credentialTargetID, time.Now().UTC(), targetMTime, targetMTime.Add(8*time.Hour), model.CredExpired)

	unsafeName := "unsafe<script>\u202e\aancillary"
	if _, err := st.DB().Exec(`UPDATE machine_registry
 SET display_name=?, hostname='PRIVATE-HOST', unix_user='PRIVATE-USER', tailscale_ip='100.64.200.10', notes='PRIVATE-NOTE'
	 WHERE machine_id IN (?,?,?)`, unsafeName, activeID, credentialTargetID, neverID); err != nil {
		t.Fatal(err)
	}
	s.hubHost = "PRIVATE-HOST"

	// Filter the table to the never-reported machine. The active machine is now
	// off-page, so its dead/double/deployment names can only be sanitized from
	// the complete StoreOverview bridge rather than Result.Items.
	body := get(t, s, "/machines?machine_id="+url.QueryEscape(neverID)+"&limit=1")
	rows := machineIndexRowsHTML(t, body)
	if !strings.Contains(rows, neverID) || strings.Contains(rows, activeID) || strings.Contains(rows, credentialTargetID) {
		t.Fatalf("fixture did not keep the ancillary machine off-page: %s", rows)
	}
	for _, want := range []string{
		`unsafe&lt;script&gt;��ancillary`,
		`display_name_control_or_format_replaced`,
		`id="double-agents"`,
		`machine_checkins.observation_age_seconds`,
		`>工具版本</h2>`,
		`部署卡住`,
		`在名冊上但從來沒有成功 check-in 過`,
		`同一家的登入這段時間在 unsafe&lt;script&gt;��ancillary 上自己續了 1 次`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("safe whole-page fixture did not exercise/render %q", want)
		}
	}
	for _, forbidden := range []string{
		unsafeName, "\u202e", "\a", "PRIVATE-HOST", "PRIVATE-USER", "100.64.200.10", "PRIVATE-NOTE",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("machine index ancillary section disclosed %q", forbidden)
		}
	}
}

func TestEnrollmentRetiredTailnetRowsUseTheSafeDisplayNameProjection(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "retired-before-corruption")
	now := time.Now().UTC()
	if err := st.RetireMachine(id, now); err != nil {
		t.Fatal(err)
	}
	unsafeName := "retired<script>\u202e\aretired"
	if _, err := st.DB().Exec(`UPDATE machine_registry
 SET display_name=?, tailscale_ip='100.64.200.9'
 WHERE machine_id=?`, unsafeName, id); err != nil {
		t.Fatal(err)
	}
	s.SetTailnetStatus(tailnet.Status{
		Available: true,
		Self: tailnet.Peer{
			Hostname: "retired-peer", IP: "100.64.200.9", Online: true,
		},
	})

	body := get(t, s, "/machines/enrollment")
	for _, want := range []string{id, `retired&lt;script&gt;��retired`, "已退役但 Tailnet 仍在線"} {
		if !strings.Contains(body, want) {
			t.Errorf("enrollment page is missing safe retired Tailnet evidence %q", want)
		}
	}
	for _, forbidden := range []string{unsafeName, "\u202e", "\a"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("enrollment page disclosed retired Tailnet display name %q", forbidden)
		}
	}
}

func TestDoubleAgentProjectionReplacesWhitespaceOnlyPrefixWithoutRewritingSentenceSpacing(t *testing.T) {
	raw := []store.DoubleAgent{{
		MachineID: "machine-a", DisplayName: "   ", Reason: "    同時有兩個 agent 在回報",
	}}
	projected := projectDoubleAgentDisplayNames(raw, map[string]string{"machine-a": "(unnamed machine)"})
	if len(projected) != 1 || projected[0].DisplayName != "(unnamed machine)" ||
		projected[0].Reason != "(unnamed machine) 同時有兩個 agent 在回報" {
		t.Fatalf("whitespace-only double-agent projection=%+v", projected)
	}
	if raw[0].DisplayName != "   " || raw[0].Reason != "    同時有兩個 agent 在回報" {
		t.Fatal("double-agent projection mutated Store-owned input")
	}
}

func TestMachineIndexGETIsReadOnly(t *testing.T) {
	s, st := newServer(t)
	onlineMachine(t, st, "read-only-a")
	onlineMachine(t, st, "read-only-b")
	tables := []string{
		"machine_registry", "enrollment_tokens", "machine_checkins", "observed_state",
		"machine_identity_hints", "machine_state_history", "operator_idempotency", "audit_log",
		"desired_state", "jobs", "hub_events",
	}
	before := tableCounts(t, st, tables...)

	body := get(t, s, "/machines?lifecycle=active&limit=1")
	index := body[strings.Index(body, `<h2 id="machines"`):]
	if !strings.Contains(index, `<form method="get" action="/machines"`) || strings.Contains(index, `method="post"`) {
		t.Fatalf("machine index did not remain a GET-only read surface: %s", index)
	}
	if next := machineIndexNextHref(body); next != "" {
		u, err := url.Parse(next)
		if err != nil {
			t.Fatal(err)
		}
		get(t, s, u.RequestURI())
	}

	after := tableCounts(t, st, tables...)
	for _, table := range tables {
		if after[table] != before[table] {
			t.Errorf("GET /machines mutated %s: before=%d after=%d", table, before[table], after[table])
		}
	}
}
