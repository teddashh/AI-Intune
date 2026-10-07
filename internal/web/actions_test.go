package web

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func verifiedWebRequest(req *http.Request, capability string) *http.Request {
	names, _ := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	return operatorauth.WithPrincipal(req, operatorauth.Principal{
		SourceAddr: "100.64.200.2", NodeStableID: "node-stable-1",
		DeviceName: "cnoderidge-ai1", TailnetUserID: "42",
		TailnetUserLogin: "operator@example.com", AuthMethod: operatorauth.AuthMethodLocalAPI,
		AuthorizedCapability: capability,
		GrantedCapabilities: operatorauth.CapabilityNames{
			View: names.View, Operate: names.Operate, Admin: names.Admin,
		},
	})
}

func postForm(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// 用 tailnet 位址保留真實 request provenance 的形狀；verified
	// principal 由下面的 context fixture 注入，handler 不應再執行 whois。
	req.RemoteAddr = "100.64.200.2:54321"
	capability := "example.com/cap/clawctl-admin"
	if strings.HasSuffix(path, "/connect") || strings.HasSuffix(path, "/continue") ||
		strings.HasSuffix(path, "/continue-preview") || strings.HasSuffix(path, "/skip-failed-batch") ||
		strings.HasSuffix(path, "/skip-failed-batch-preview") || strings.HasSuffix(path, "/retry") ||
		strings.HasSuffix(path, "/retry-preview") || strings.HasSuffix(path, "/diagnostic-noop-preview") ||
		strings.HasSuffix(path, "/diagnostic-noop-jobs") {
		capability = "example.com/cap/clawctl-operate"
	}
	req = verifiedWebRequest(req, capability)
	mux.ServeHTTP(rec, req)
	return rec
}

func hiddenFormValue(t *testing.T, body, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`)
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 || match[1] == "" {
		t.Fatalf("rendered form 缺少 hidden %s：\n%s", name, body)
	}
	return match[1]
}

// previewEnrollForm returns the exact create form rendered by the preview.
// Keeping the generated request key here is important: a helper that minted a
// fresh key for create/retry would hide the one-response-only recovery path.
func previewEnrollForm(t *testing.T, s *Server, name, reason string) (*httptest.ResponseRecorder, url.Values) {
	t.Helper()
	preview := postForm(t, s, "/enrollments/preview", url.Values{
		"name": {name}, "reason": {reason},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("enroll preview = %d：%s", preview.Code, preview.Body.String())
	}
	form := url.Values{
		"name":            {name},
		"reason":          {strings.TrimSpace(reason)},
		"preview_digest":  {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"idempotency_key": {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
	}
	return preview, form
}

func createEnrollment(t *testing.T, s *Server, name, reason string) (*httptest.ResponseRecorder, url.Values) {
	t.Helper()
	_, form := previewEnrollForm(t, s, name, reason)
	return postForm(t, s, "/enrollments", form), form
}

// previewEnrollTokenRevocationForm keeps the exact digest and request key
// rendered by the review page. Replacing either value on confirmation would
// accidentally test a different operator decision instead of browser retry.
func previewEnrollTokenRevocationForm(t *testing.T, s *Server, machineID, reason string) (*httptest.ResponseRecorder, url.Values) {
	t.Helper()
	preview := postForm(t, s, "/machines/"+machineID+"/revoke-token/preview", url.Values{
		"reason": {reason},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("enrollment ticket revocation preview = %d：%s", preview.Code, preview.Body.String())
	}
	form := url.Values{
		"reason":          {strings.TrimSpace(reason)},
		"preview_digest":  {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"idempotency_key": {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
	}
	return preview, form
}

// previewMachineLifecycleForm keeps the revision, digest, and request key from
// the rendered review page together.  Creating any of them again for apply
// would turn a browser retry into a new operator decision.
func previewMachineLifecycleForm(t *testing.T, s *Server, machineID string,
	desired store.MachineLifecycleState, reason string,
) (*httptest.ResponseRecorder, url.Values) {
	t.Helper()
	machine, err := s.store.GetMachine(machineID)
	if err != nil {
		t.Fatalf("lifecycle preview machine: %v", err)
	}
	preview := postForm(t, s, "/machines/"+machineID+"/lifecycle-preview", url.Values{
		"desired_state":     {string(desired)},
		"expected_revision": {strconv.FormatInt(machine.LifecycleRevision, 10)},
		"reason":            {reason},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("machine lifecycle preview = %d：%s", preview.Code, preview.Body.String())
	}
	form := url.Values{
		"desired_state":     {string(desired)},
		"expected_revision": {hiddenFormValue(t, preview.Body.String(), "expected_revision")},
		"preview_digest":    {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"idempotency_key":   {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
		"reason":            {strings.TrimSpace(reason)},
	}
	return preview, form
}

func doGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := verifiedWebRequest(httptest.NewRequest("GET", path, nil), "example.com/cap/clawctl-view")
	mux.ServeHTTP(rec, req)
	return rec
}

// TestWritePathsRefuseGET 是這一組裡最重要的測試。
//
// 一個 `GET /machines/x/retire` 會在下面任何一件事發生時執行：
// 把連結貼進 Telegram（預覽抓取）、iOS 的 Safari 預先載入、
// 任何一個做 link prefetch 的東西經過那一頁。
//
// 所以寫入路徑對 GET 的正確反應是 **405**，不是「做一半」。
func TestWritePathsRefuseGET(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	for _, path := range []string{
		"/machines/" + id + "/retire",
		"/machines/" + id + "/unretire",
		"/machines/" + id + "/connect",
		"/machines/" + id + "/channel",
		"/machines/" + id + "/display-name-preview",
		"/machines/" + id + "/display-name",
		"/machines/" + id + "/notes-preview",
		"/machines/" + id + "/notes",
		"/deployments/deployment-id/continue-preview",
		"/deployments/deployment-id/continue",
		"/deployments/deployment-id/skip-failed-batch-preview",
		"/deployments/deployment-id/skip-failed-batch",
		"/deployments/deployment-id/retry-preview",
		"/deployments/deployment-id/retry",
		"/deployments/deployment-id/abandon-preview",
		"/deployments/deployment-id/abandon",
		"/jobs/job-id/verifier-assignment-preview",
		"/jobs/job-id/verifier-assignments",
		"/settings/tailnet/peer-ignore-preview",
		"/settings/tailnet/peer-ignores",
		"/tenant/maintenance/retention/prune-preview",
		"/tenant/maintenance/retention/prunes",
	} {
		rec := doGet(t, s, path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d，應該是 405 —— 一個 GET 就能改狀態的網址，"+
				"會在有人把它貼進聊天室的那一刻自己執行", path, rec.Code)
		}
	}
	// 而且真的什麼都沒改。
	m, err := st.GetMachine(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.RetiredAt != nil {
		t.Error("GET 之後那台機器竟然退役了")
	}
	for _, table := range []string{"deployments", "desired_state", "jobs", "operator_idempotency"} {
		if got := tableCounts(t, st, table)[table]; got != 0 {
			t.Errorf("GET deployment action mutated %s: count=%d", table, got)
		}
	}
}

func TestMachineRenameWebReviewApplyAndReplayUseCanonicalWorkflow(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "rename-web-before")
	page := doGet(t, s, "/machines/"+id+"?section=actions")
	if page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), `action="/machines/`+id+`/display-name-preview"`) ||
		!strings.Contains(page.Body.String(), "machine ID、agent 與機器上的 hostname 都不變") {
		t.Fatalf("machine page=%d body=%s", page.Code, page.Body.String())
	}
	preview := postForm(t, s, "/machines/"+id+"/display-name-preview", url.Values{
		"display_name": {"rename-web-after"}, "reason": {"align registry label"},
	})
	if preview.Code != http.StatusOK || preview.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(preview.Body.String(), "確認重新命名 rename-web-before") ||
		!strings.Contains(preview.Body.String(), "名稱型 expectations 改用新名稱") {
		t.Fatalf("preview=%d headers=%v body=%s", preview.Code, preview.Header(), preview.Body.String())
	}
	form := url.Values{
		"display_name":         {"rename-web-after"},
		"confirm_display_name": {"rename-web-before"},
		"preview_digest":       {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"reason":               {"align registry label"},
		"idempotency_key":      {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
	}
	apply := postForm(t, s, "/machines/"+id+"/display-name", form)
	if apply.Code != http.StatusSeeOther || apply.Header().Get("Location") != "/machines/"+id+"?section=actions#actions" {
		t.Fatalf("apply=%d headers=%v body=%s", apply.Code, apply.Header(), apply.Body.String())
	}
	machine, err := st.GetMachine(id)
	if err != nil || machine.DisplayName != "rename-web-after" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	replay := postForm(t, s, "/machines/"+id+"/display-name", form)
	if replay.Code != http.StatusSeeOther {
		t.Fatalf("replay=%d body=%s", replay.Code, replay.Body.String())
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditMachineRename || !entry.OK || entry.SourceKind != operator.SourceKindWeb {
			t.Fatalf("audit entry=%+v", entry)
		}
	}
}

func TestMachineNotesWebReviewApplyAndReplayDoNotCopyTextToAudit(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "notes-web")
	page := doGet(t, s, "/machines/"+id+"?section=actions")
	if page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), `action="/machines/`+id+`/notes-preview"`) ||
		!strings.Contains(page.Body.String(), "機器設定與 agent 不變") {
		t.Fatalf("machine page=%d body=%s", page.Code, page.Body.String())
	}
	preview := postForm(t, s, "/machines/"+id+"/notes-preview", url.Values{
		"notes": {"GPU runner"}, "reason": {"record machine purpose"},
	})
	if preview.Code != http.StatusOK || preview.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(preview.Body.String(), "確認名冊備註 notes-web") ||
		!strings.Contains(preview.Body.String(), "GPU runner") ||
		!strings.Contains(preview.Body.String(), "machine ID、機器設定與 agent 不變") {
		t.Fatalf("preview=%d headers=%v body=%s", preview.Code, preview.Header(), preview.Body.String())
	}
	form := url.Values{
		"notes":                {"GPU runner"},
		"confirm_display_name": {"notes-web"},
		"preview_digest":       {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"reason":               {"record machine purpose"},
		"idempotency_key":      {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
	}
	apply := postForm(t, s, "/machines/"+id+"/notes", form)
	if apply.Code != http.StatusSeeOther || apply.Header().Get("Location") != "/machines/"+id+"?section=actions#actions" {
		t.Fatalf("apply=%d headers=%v body=%s", apply.Code, apply.Header(), apply.Body.String())
	}
	machine, err := st.GetMachine(id)
	if err != nil || machine.Notes != "GPU runner" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	replay := postForm(t, s, "/machines/"+id+"/notes", form)
	if replay.Code != http.StatusSeeOther {
		t.Fatalf("replay=%d body=%s", replay.Code, replay.Body.String())
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditMachineNotes || !entry.OK || entry.SourceKind != operator.SourceKindWeb ||
			strings.Contains(entry.Detail, "GPU runner") {
			t.Fatalf("audit entry=%+v", entry)
		}
	}
}

func TestMachineChannelNeedsNameAndRejectsNeverObservedMachine(t *testing.T) {
	s, st := newServer(t)
	id := enroll(t, st, "sampleagent1", time.Now().UTC())

	rec := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"stable"}, "confirm": {"sampleagent1"}, "expected_revision": {"0"}, "idempotency_key": {"never-observed"},
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "machine has never reported") {
		t.Fatalf("未觀測機器指派 channel = %d：%s", rec.Code, rec.Body.String())
	}
	if m, _ := st.GetMachine(id); m.Channel != "" {
		t.Fatalf("結構性拒絕後 channel=%q", m.Channel)
	}

	seen := onlineMachine(t, st, "cnode-channel")
	wrong := postForm(t, s, "/machines/"+seen+"/channel", url.Values{
		"channel": {"canary"}, "confirm": {"cnode"}, "expected_revision": {"0"}, "idempotency_key": {"wrong-name"},
	})
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("名字打錯 = %d，預期 400", wrong.Code)
	}
	ok := postForm(t, s, "/machines/"+seen+"/channel", url.Values{
		"channel": {"canary"}, "confirm": {"cnode-channel"}, "expected_revision": {"0"}, "idempotency_key": {"channel-ok"},
	})
	if ok.Code != http.StatusSeeOther {
		t.Fatalf("指派成功 = %d：%s", ok.Code, ok.Body.String())
	}
	entries, _ := st.Audit(seen, 10)
	if len(entries) < 1 || entries[0].Action != store.AuditMachineChannel || entries[0].Reason != "未指派 → canary" || !entries[0].OK {
		t.Fatalf("channel audit 不對：%+v", entries)
	}
}

func TestMachineChannelActionRejectsMoveWhileJobIsNonterminal(t *testing.T) {
	s, st := newServer(t)
	_, jobs, ids := webDeployment(t, st, "canary", "cnode-busy")
	if len(jobs) != 1 {
		t.Fatalf("active deployment jobs=%+v", jobs)
	}
	id := ids["cnode-busy"]
	rec := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"stable"}, "confirm": {"cnode-busy"}, "expected_revision": {"1"}, "idempotency_key": {"busy-channel"},
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "未終態 job") {
		t.Fatalf("active-job channel move = %d：%s", rec.Code, rec.Body.String())
	}
	m, err := st.GetMachine(id)
	if err != nil || m.Channel != "canary" {
		t.Fatalf("rejected web action changed channel: machine=%+v err=%v", m, err)
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) == 0 || entries[0].Action != store.AuditMachineChannel || entries[0].OK ||
		!strings.Contains(entries[0].Detail, "未終態 job") {
		t.Fatalf("web rejection audit=%+v err=%v", entries, err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, time.Now().UTC().Format(time.RFC3339Nano), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	// Reposting the same rendered form preserves the original rejection.
	replay := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"stable"}, "confirm": {"cnode-busy"}, "expected_revision": {"1"}, "idempotency_key": {"busy-channel"},
	})
	if replay.Code != http.StatusConflict || !strings.Contains(replay.Body.String(), "未終態 job") ||
		!strings.Contains(replay.Body.String(), "原 request 的回放判決") {
		t.Fatalf("same form did not replay original rejection=%d: %s", replay.Code, replay.Body.String())
	}
	// A reload mints a new key, so the now-valid attempt can run.
	afterReload := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"stable"}, "confirm": {"cnode-busy"}, "expected_revision": {"1"}, "idempotency_key": {"busy-channel-after-reload"},
	})
	if afterReload.Code != http.StatusSeeOther {
		t.Fatalf("fresh rendered form could not retry=%d: %s", afterReload.Code, afterReload.Body.String())
	}
	m, err = st.GetMachine(id)
	if err != nil || m.Channel != "stable" || m.ChannelRevision != 2 {
		t.Fatalf("fresh form did not change channel: machine=%+v err=%v", m, err)
	}
}

func TestMachineChannelActionRejectsStaleRenderedRevision(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "cnode-stale-form")
	first := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"canary"}, "confirm": {"cnode-stale-form"}, "expected_revision": {"0"}, "idempotency_key": {"stale-first"},
	})
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first form submit=%d: %s", first.Code, first.Body.String())
	}
	stale := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"stable"}, "confirm": {"cnode-stale-form"}, "expected_revision": {"0"}, "idempotency_key": {"stale-second"},
	})
	if stale.Code != http.StatusPreconditionFailed || !strings.Contains(stale.Body.String(), "revision") {
		t.Fatalf("stale form submit=%d: %s", stale.Code, stale.Body.String())
	}
	m, err := st.GetMachine(id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("stale form changed machine=%+v err=%v", m, err)
	}
}

func TestMachineChannelPageMintsFreshIdempotencyKeyPerRender(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "cnode-form-key")
	keyPattern := regexp.MustCompile(`name="idempotency_key" value="([^"]+)"`)
	first := keyPattern.FindStringSubmatch(get(t, s, "/machines/"+id))
	second := keyPattern.FindStringSubmatch(get(t, s, "/machines/"+id))
	if len(first) != 2 || len(second) != 2 || first[1] == "" || second[1] == "" {
		t.Fatalf("rendered keys missing: first=%v second=%v", first, second)
	}
	if first[1] == second[1] {
		t.Fatalf("page reload reused idempotency key %q", first[1])
	}
}

func TestMachineChannelFormRequiresRenderedIdempotencyKey(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "cnode-no-key")
	rec := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"canary"}, "confirm": {"cnode-no-key"}, "expected_revision": {"0"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Idempotency-Key") {
		t.Fatalf("missing rendered key=%d: %s", rec.Code, rec.Body.String())
	}
}

func TestMachineChannelFormRequiresExplicitChannel(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "cnode-explicit-channel")
	if err := st.SetMachineChannel(id, "canary"); err != nil {
		t.Fatal(err)
	}
	rec := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"confirm": {"cnode-explicit-channel"}, "expected_revision": {"1"},
		"idempotency_key": {"missing-channel-form"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "channel") {
		t.Fatalf("missing channel form=%d: %s", rec.Code, rec.Body.String())
	}
	m, err := st.GetMachine(id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("missing channel cleared assignment: machine=%+v err=%v", m, err)
	}
}

func TestRetiredMachineChannelIsReadOnlyInUIAndCraftedPostIsAudited(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "cnode-retired-channel")
	if err := st.SetMachineChannel(id, "canary"); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	page := doGet(t, s, "/machines/"+id).Body.String()
	if !strings.Contains(page, "已退役") || !strings.Contains(page, "唯讀") ||
		strings.Contains(page, `action="/machines/`+id+`/channel"`) {
		t.Fatalf("retired machine channel UI is not read-only: %s", page)
	}
	rec := postForm(t, s, "/machines/"+id+"/channel", url.Values{
		"channel": {"stable"}, "confirm": {"cnode-retired-channel"},
		"expected_revision": {"1"}, "idempotency_key": {"retired-crafted-post"},
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "退役") {
		t.Fatalf("crafted retired channel POST=%d: %s", rec.Code, rec.Body.String())
	}
	machine, err := st.GetMachine(id)
	if err != nil || machine.Channel != "canary" || machine.ChannelRevision != 1 {
		t.Fatalf("crafted retired POST changed channel: machine=%+v err=%v", machine, err)
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 1 || entries[0].OK ||
		entries[0].IdempotencyKey != "retired-crafted-post" || !strings.Contains(entries[0].Detail, "退役") {
		t.Fatalf("crafted retired POST audit=%+v err=%v", entries, err)
	}
}

// A stale/bookmarked form can target a machine that no longer resolves. It is
// still an operator write attempt and must reach the canonical service so the
// rejection is auditable; returning an early 404 would silently drop it.
func TestMachineChannelMissingMachineAttemptIsAudited(t *testing.T) {
	s, st := newServer(t)
	rec := postForm(t, s, "/machines/missing-machine/channel", url.Values{
		"channel": {"canary"}, "confirm": {"missing-machine"},
		"expected_revision": {"0"}, "idempotency_key": {"missing-machine-attempt"},
	})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "找不到 machine") {
		t.Fatalf("missing machine write=%d: %s", rec.Code, rec.Body.String())
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 || entries[0].OK ||
		entries[0].IdempotencyKey != "missing-machine-attempt" {
		t.Fatalf("missing machine attempt audit=%+v err=%v", entries, err)
	}
}

// TestRetireNeedsTheNameTypedRight
//
// ⚠ 打字確認不是為了防惡意，是為了讓「按錯」需要一個刻意的動作。
// 而打錯的那一次也要進 audit：一筆「有人差一點退役了 samplehub1」的紀錄，
// 比一個安靜的錯誤畫面有用得多。
func TestRetireNeedsTheNameTypedRight(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	_, form := previewMachineLifecycleForm(t, s, id, store.MachineLifecycleRetired, "要搬機房")
	form.Set("confirm", "cnode")
	rec := postForm(t, s, "/machines/"+id+"/retire", form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("確認打錯時應該回 400 說明，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "confirm_display_name 必須") {
		t.Errorf("沒有講清楚為什麼沒動作：\n%s", rec.Body.String())
	}

	m, err := st.GetMachine(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.RetiredAt != nil {
		t.Fatal("確認欄位打錯，機器卻退役了")
	}

	entries, err := st.Audit(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("失敗的那一次沒有進 audit（拿到 %d 筆）", len(entries))
	}
	if entries[0].OK {
		t.Error("audit 把一次沒發生的退役記成成功")
	}
	if entries[0].Action != store.AuditMachineLifecycle ||
		!strings.Contains(entries[0].Detail, store.OperatorCodeConfirmationMismatch) {
		t.Errorf("audit 沒留下 canonical lifecycle rejection：%+v", entries[0])
	}
	if entries[0].Reason != "要搬機房" {
		t.Errorf("理由原文沒留住：%q", entries[0].Reason)
	}
}

// TestRetireNeedsAReason：半年後看到這一筆的人需要知道當初為什麼。
func TestRetireNeedsAReason(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	_, form := previewMachineLifecycleForm(t, s, id, store.MachineLifecycleRetired, "要搬機房")
	form.Set("reason", "")
	form.Set("confirm", "samplehub1")
	rec := postForm(t, s, "/machines/"+id+"/retire", form)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reason 不可省略") {
		t.Errorf("沒填理由卻沒有擋下來：\n%s", rec.Body.String())
	}
	if m, _ := st.GetMachine(id); m.RetiredAt != nil {
		t.Error("沒填理由，機器卻退役了")
	}

	entries, err := st.Audit(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("失敗的那一次沒有進 audit（拿到 %d 筆）", len(entries))
	}
	if entries[0].OK {
		t.Error("audit 把一次沒發生的退役記成成功")
	}
	if entries[0].Action != store.AuditMachineLifecycle ||
		!strings.Contains(entries[0].Detail, store.OperatorCodeReasonRequired) {
		t.Errorf("audit 沒留下 canonical lifecycle rejection：%+v", entries[0])
	}
}

// TestRetireThenUnretireIsFullyReversible
//
// ⚠⚠ 這是這整組寫入路徑能存在的前提。
//
// retire 改的是**分母**，而「機器不准從名冊上消失」正好是這個產品
// 存在的理由。一個按下去就回不來的 retire 按鈕，是在這個東西自己的
// 核心承諾上開一個洞。Operator auth 不會阻止已授權的人按錯，
// 所以可反悔性仍是這條寫入路徑存在的前提。
func TestRetireThenUnretireIsFullyReversible(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	// 退役。
	_, retireForm := previewMachineLifecycleForm(t, s, id, store.MachineLifecycleRetired, "雲端商要收回這台")
	retireForm.Set("confirm", "samplehub1")
	rec := postForm(t, s, "/machines/"+id+"/retire", retireForm)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("退役成功應該轉回機器頁，得到 %d：%s", rec.Code, rec.Body.String())
	}
	m, err := st.GetMachine(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.RetiredAt == nil {
		t.Fatal("退役沒生效")
	}
	// 離開分母。
	if ov, err := st.Overview(time.Now().UTC()); err != nil {
		t.Fatal(err)
	} else if ov.Expected != 0 {
		t.Errorf("退役後分母還是 %d", ov.Expected)
	}
	// 機器頁上要看得到「已經退場」跟放回去的按鈕。
	body := doGet(t, s, "/machines/"+id).Body.String()
	if !strings.Contains(body, "這台已經退場") {
		t.Error("機器頁沒有講出它已經退場")
	}
	if !strings.Contains(body, "/lifecycle-preview") || !strings.Contains(body, "預覽恢復管理") {
		t.Error("退場之後畫面上沒有原生 preview 回頭路 —— 那就等於不可逆")
	}

	// 放回來。恢復會重新允許保留的 bearer / pending ticket，因此同樣
	// 必須 preview、固定 revision 與 impact，並逐字確認 display name。
	_, restoreForm := previewMachineLifecycleForm(t, s, id, store.MachineLifecycleActive, "按錯了")
	restoreForm.Set("confirm", "samplehub1")
	rec = postForm(t, s, "/machines/"+id+"/unretire", restoreForm)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("取消退役失敗：%d %s", rec.Code, rec.Body.String())
	}
	if m, _ := st.GetMachine(id); m.RetiredAt != nil {
		t.Fatal("取消退役沒生效")
	}
	if ov, err := st.Overview(time.Now().UTC()); err != nil {
		t.Fatal(err)
	} else if ov.Expected != 1 {
		t.Errorf("放回來之後分母是 %d，應該是 1", ov.Expected)
	}

	// 兩個動作都在 audit 裡，理由原文都在。
	entries, err := st.Audit(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit 有 %d 筆，應該是 2", len(entries))
	}
	if entries[0].Action != store.AuditMachineLifecycle || entries[1].Action != store.AuditMachineLifecycle {
		t.Errorf("audit 順序不對（要由新到舊）：%s, %s", entries[0].Action, entries[1].Action)
	}
	for _, e := range entries {
		if !e.OK {
			t.Errorf("%s 被記成失敗", e.Action)
		}
	}
	if entries[1].Reason != "雲端商要收回這台" {
		t.Errorf("退役理由原文沒留住：%q", entries[1].Reason)
	}
	if entries[0].Reason != "按錯了" || !strings.Contains(entries[0].Detail, "retired→active") ||
		!strings.Contains(entries[1].Detail, "active→retired") {
		t.Errorf("lifecycle audit 沒留下兩個方向的 canonical receipt：%+v", entries)
	}
}

// TestConnectRecordsEvenWhenItCannotConnect
//
// ⚠ 「有人試著連 sampleagent2，而它綁在 localhost」正好是事後最想知道的那種事：
// 它會告訴你哪一台該去改設定了。只記成功的 audit 答不出這件事。
func TestConnectMissingMachineUsesCanonicalOperatorAudit(t *testing.T) {
	s, st := newServer(t)
	rec := postForm(t, s, "/machines/missing-connect/connect",
		url.Values{"reason": {"must not cross the detail boundary"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing Connect status=%d body=%s", rec.Code, rec.Body.String())
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing Connect audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditConnect || entry.MachineID != "missing-connect" ||
		entry.Subject != "missing-connect" || entry.OK || entry.Reason != "" ||
		entry.Detail != "operator machine detail unavailable" ||
		entry.AuthSubject != "tailscale-user:42" || entry.SourceKind != operator.SourceKindWeb {
		t.Fatalf("missing Connect canonical audit=%+v", entry)
	}
}

func TestConnectRecordsEvenWhenItCannotConnect(t *testing.T) {
	s, st := newServer(t)
	id := enroll(t, st, "sampleagent2", time.Now().UTC().Add(-time.Hour))
	now := time.Now().UTC()
	b := batch(now)
	b.BAT.Bind, b.BAT.ListenAddrs = "localhost", []string{"127.0.0.1"}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatal(err)
	}
	page := get(t, s, "/machines/"+id)
	if !strings.Contains(page, "BAT evidence clocks") || !strings.Contains(page, "Hub ") ||
		strings.Contains(page, `action="/machines/`+id+`/connect"`) {
		t.Fatalf("unavailable BAT page lost typed clocks or exposed connect form: %s", page)
	}

	rec := postForm(t, s, "/machines/"+id+"/connect",
		url.Values{"reason": {"查 grok 的登入"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("連不上時應該回一頁說明，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "連不進來") {
		t.Errorf("沒有把連不上的原因講出來：\n%s", rec.Body.String())
	}

	entries, err := st.Audit(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].OK || entries[0].Reason != "查 grok 的登入" {
		t.Fatalf("連不上的那一次沒有被記成失敗：%+v", entries)
	}
	if !strings.Contains(entries[0].Detail, "localhost") {
		t.Errorf("audit 沒留下連不上的原因：%q", entries[0].Detail)
	}
}

// TestConnectRedirectsToTheMeasuredAddress
// ⚠ 轉址之後 Hub 就離開那條路徑了。它不是代理，這裡驗的是「它只轉址」。
func TestConnectRedirectsToTheMeasuredAddress(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	rec := postForm(t, s, "/machines/"+id+"/connect", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("應該 303 轉過去，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "https://100.64.0.1:9876" {
		t.Errorf("轉到了 %q，應該是量出來的那個位址", got)
	}
	// 沒填理由也要留紀錄 —— 記帳不可以是連線的前置條件。
	entries, _ := st.Audit(id, 10)
	if len(entries) != 1 || !entries[0].OK {
		t.Fatalf("成功的連線沒有進 audit：%+v", entries)
	}
	// ⚠ 「誰」那一欄要是 Tailscale 給的答案，不是這個 console 自己編的。
	if entries[0].WhoNode != "cnoderidge-ai1" {
		t.Errorf("audit 沒記下 tailscale 認出來的節點：%+v", entries[0])
	}
	if entries[0].SourceAddr != "100.64.200.2" {
		t.Errorf("來源位址不對：%q", entries[0].SourceAddr)
	}
	if entries[0].AuthSubject != "tailscale-user:42" || entries[0].AuthNodeID != "node-stable-1" ||
		entries[0].AuthCapability != "example.com/cap/clawctl-operate" ||
		entries[0].AuthMethod != operatorauth.AuthMethodLocalAPI ||
		entries[0].AuthDecision != string(operatorauth.Authorized) ||
		entries[0].SourceKind != "web" {
		t.Errorf("verified operator evidence missing from audit: %+v", entries[0])
	}
	// 而且畫面上看得到。
	if body := doGet(t, s, "/audit").Body.String(); !strings.Contains(body, "cnoderidge-ai1") {
		t.Error("動作紀錄那一頁上看不到是從哪一台按的")
	}
}

func TestConnectRedirectDoesNotDependOnObservationalAuditWrite(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "connect-audit-unavailable")
	if _, err := st.DB().Exec(`CREATE TRIGGER reject_connect_audit
BEFORE INSERT ON audit_log WHEN NEW.action = 'connect'
BEGIN SELECT RAISE(ABORT, 'connect audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}

	rec := postForm(t, s, "/machines/"+id+"/connect", url.Values{"reason": {"open console"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://100.64.0.1:9876" {
		t.Fatalf("observational audit failure blocked Connect: status=%d headers=%v body=%s",
			rec.Code, rec.Header(), rec.Body.String())
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected Connect audit unexpectedly persisted: entries=%+v err=%v", entries, err)
	}
}

func TestConnectUndecodableObservationFailsClosedWithoutRenderingRawPayload(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "typed-connect-invalid")
	privatePayload := `{"running":true,"argv":"PRIVATE_BAT_ARGV"`
	if _, err := st.DB().Exec(`UPDATE observed_state SET payload=? WHERE machine_id=? AND kind=?`,
		privatePayload, id, store.KindBAT); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	for _, want := range []string{
		"BAT 連線座標無效", "BAT 連線位址不可用",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("invalid BAT page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, privatePayload) || strings.Contains(body, "PRIVATE_BAT_ARGV") ||
		strings.Contains(body, `action="/machines/`+id+`/connect"`) {
		t.Fatal("invalid BAT payload or actionable connect form escaped typed projection")
	}

	rec := postForm(t, s, "/machines/"+id+"/connect", url.Values{"reason": {"inspect invalid BAT"}})
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" ||
		strings.Contains(rec.Body.String(), "PRIVATE_BAT_ARGV") {
		t.Fatalf("invalid BAT connect did not fail closed: status=%d headers=%v body=%s",
			rec.Code, rec.Header(), rec.Body.String())
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 1 || entries[0].OK ||
		!strings.Contains(entries[0].Detail, "typed") || strings.Contains(entries[0].Detail, "PRIVATE_BAT_ARGV") {
		t.Fatalf("invalid BAT audit=%+v err=%v", entries, err)
	}
}

// TestAuditPageSaysWhatItDoesNotKnow
//
// ⚠⚠ 這一頁最重要的內容是它的免責範圍，而那不是免責聲明：
// BAT 的位址在機器頁上是可複製的純文字，人隨時可以繞過按鈕直接連 ——
// 所以這一頁的空白**不代表沒有人連過去**。
// 一份宣稱自己記下了全部連線的 audit 是在說謊。
func TestAuditPageKeepsBoundaryDetailsOutOfTheTaskFlow(t *testing.T) {
	s, _ := newServer(t)
	body := doGet(t, s, "/audit").Body.String()

	for _, forbidden := range []string{
		"不代表沒有人連過去",  // 空白的意思
		"WhoIsForIP", // 「誰」與 capability 是上游 LocalAPI 給的
		"不是重新確認此刻坐在鍵盤前的人",
		"不是 fresh MFA",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("動作紀錄仍顯示防禦性文案 %q", forbidden)
		}
	}
}

// TestEnrollTokenAppearsExactlyOnceAndNeverInTheAudit
//
// ⚠⚠ 這是這一組裡最重要的測試。
//
// 發票跟 retire 的風險是**不同類的**：retire 按錯了可以 unretire，
// 狀態回得去。一張畫在畫面上的 token 收不回來 —— 它在被畫出來的那一刻
// 就已經進了瀏覽器歷史。撤銷能讓它不能再兌換，不能讓它消失。
//
// 所以明文只准出現在那一頁上，而且不准漏進 audit、不准漏進任何別的頁面。
// 一個把憑證寫進稽核紀錄的系統，是把稽核變成第二個外洩點。
func TestEnrollPreviewIsNonMutatingAndFreezesOneRequestKey(t *testing.T) {
	s, st := newServer(t)

	preview, form := previewEnrollForm(t, s, "sampleagent5", "  新開的機器  ")
	body := preview.Body.String()
	for _, want := range []string{
		"建立前預覽", "2h0m0s", "從未報到",
		"新開的機器", `action="/enrollments"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview 沒有呈現 %q：\n%s", want, body)
		}
	}
	if digest := form.Get("preview_digest"); !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("preview digest=%q", digest)
	}
	key := form.Get("idempotency_key")
	if !strings.HasPrefix(key, "web-enroll-token-") {
		t.Errorf("rendered idempotency key=%q", key)
	}
	if got := strings.Count(body, key); got != 1 {
		t.Errorf("同一張 preview 應只凍結一把 request key，畫面出現 %d 次", got)
	}
	if got := form.Get("reason"); got != "新開的機器" {
		t.Errorf("preview 沒有凍結 operator 看過的 reason：%q", got)
	}

	ov, err := st.Overview(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if ov.Expected != 0 || len(ov.Machines) != 0 {
		t.Fatalf("preview 改了名冊：%+v", ov)
	}
	entries, err := st.Audit("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("preview 寫了 audit：%+v", entries)
	}

	missingPreview := postForm(t, s, "/enrollments", url.Values{
		"name": {"sampleagent6"}, "idempotency_key": {"web-enroll-token-no-preview"},
	})
	if missingPreview.Code != http.StatusPreconditionRequired ||
		!strings.Contains(missingPreview.Body.String(), "preview") {
		t.Fatalf("create 沒有 preview = %d：%s", missingPreview.Code, missingPreview.Body.String())
	}
}

func TestEnrollTokenAppearsExactlyOnceAndNeverInTheAudit(t *testing.T) {
	s, st := newServer(t)

	rec, createForm := createEnrollment(t, s, "sampleagent5", "新開的機器")
	if rec.Code != http.StatusOK {
		t.Fatalf("開票應該回一頁，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("一次性 token response 沒有 no-store：%q", rec.Header().Get("Cache-Control"))
	}
	body := rec.Body.String()

	// 把畫面上那串 token 撈出來 —— 不是自己編一個，因為要驗的正是
	// 「畫面上這一串」有沒有洩到別的地方去。
	m := regexp.MustCompile(`(?s)<pre class="copy">([A-Za-z0-9_\-]{16,})</pre>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("開票那一頁上找不到 token：\n%s", body)
	}
	tok := m[1]
	if got := strings.Count(body, tok); got != 1 {
		t.Fatalf("fresh response 的 token 明文在整頁出現 %d 次，應恰好一次", got)
	}
	if !strings.Contains(body, "./install-agent.sh --hub") || strings.Contains(body, "--token '") {
		t.Fatal("agent bootstrap command 沒有使用互動式 secret 輸入")
	}

	if !strings.Contains(body, "只顯示一次") {
		t.Error("沒有講出這串字只出現一次")
	}
	if !strings.Contains(body, "撤銷只影響 pending ticket") {
		t.Error("沒有列出撤銷影響")
	}
	// 開票改變了分母，那件事必須寫在畫面上，否則會被當成 bug。
	if !strings.Contains(body, "已加入名冊") {
		t.Error("沒有講出開票就進分母")
	}

	// --- token 不准進 audit。
	entries, err := st.Audit("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("開票沒有進 audit（%d 筆）", len(entries))
	}
	e := entries[0]
	if e.Action != store.AuditEnrollToken || !e.OK || e.Subject != "sampleagent5" {
		t.Errorf("audit 記錯了：%+v", e)
	}
	if e.Reason != "新開的機器" || e.IdempotencyKey != createForm.Get("idempotency_key") ||
		!strings.HasPrefix(e.RequestDigest, "sha256:") || e.SourceKind != operator.SourceKindWeb {
		t.Errorf("operator audit correlation/provenance 記錯了：%+v", e)
	}
	for _, field := range []string{e.Subject, e.Reason, e.Detail, e.WhoNode, e.WhoUser} {
		if strings.Contains(field, tok) {
			t.Errorf("token 明文漏進 audit 了：%q", field)
		}
	}

	// --- token 不准出現在別的頁面上。
	id := e.MachineID
	if id == "" {
		t.Fatal("audit 沒記下 machine_id —— 那就找不回這台了")
	}
	for _, path := range []string{"/", "/machines/" + id, "/audit"} {
		if strings.Contains(doGet(t, s, path).Body.String(), tok) {
			t.Errorf("%s 上出現了 token 明文", path)
		}
	}
}

func TestParallelEnrollCreateWithSameKeyReturnsOneSecretAndOneRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := New(st, "")
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	_, form := previewEnrollForm(t, s, "onode-parallel", "parallel create")
	mux := http.NewServeMux()
	s.Routes(mux)

	type response struct {
		recorder *httptest.ResponseRecorder
	}
	responses := make(chan response, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/enrollments", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "100.64.200.2:54321"
		req = verifiedWebRequest(req, "example.com/cap/clawctl-admin")
		go func() {
			<-start
			mux.ServeHTTP(recorder, req)
			responses <- response{recorder: recorder}
		}()
	}
	close(start)

	var freshCount, recoveryCount int
	secretPattern := regexp.MustCompile(`(?s)<pre class="copy">([A-Za-z0-9_\-]{16,})</pre>`)
	for i := 0; i < 2; i++ {
		rec := (<-responses).recorder
		if rec.Code != http.StatusOK {
			t.Fatalf("parallel create response %d = %d：%s", i, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("parallel create response %d 沒有 no-store：%q", i, rec.Header().Get("Cache-Control"))
		}
		body := rec.Body.String()
		if match := secretPattern.FindStringSubmatch(body); match != nil {
			freshCount++
			if got := strings.Count(body, match[1]); got != 1 {
				t.Fatalf("parallel fresh response 的 token 在整頁出現 %d 次", got)
			}
			continue
		}
		if strings.Contains(body, "原票已建立；token 無法重顯") &&
			strings.Contains(body, "預覽撤銷原本的 pending enrollment ticket") {
			recoveryCount++
			continue
		}
		t.Fatalf("parallel create response 既不是 fresh 也不是 redacted recovery：%s", body)
	}
	if freshCount != 1 || recoveryCount != 1 {
		t.Fatalf("parallel same-key create: fresh=%d recovery=%d，應各一", freshCount, recoveryCount)
	}

	readerURL := &url.URL{Scheme: "file", Path: dbPath}
	readerQuery := readerURL.Query()
	readerQuery.Set("mode", "ro")
	readerQuery.Add("_pragma", "query_only(1)")
	readerQuery.Add("_pragma", "busy_timeout(5000)")
	readerURL.RawQuery = readerQuery.Encode()
	reader, err := sql.Open("sqlite", readerURL.String())
	if err != nil {
		t.Fatalf("open verification reader: %v", err)
	}
	t.Cleanup(func() { reader.Close() })
	var machines, tokens, idempotency int
	if err := reader.QueryRow(`SELECT
		(SELECT COUNT(*) FROM machine_registry),
		(SELECT COUNT(*) FROM enrollment_tokens),
		(SELECT COUNT(*) FROM operator_idempotency)`).Scan(&machines, &tokens, &idempotency); err != nil {
		t.Fatalf("count parallel create state: %v", err)
	}
	if machines != 1 || tokens != 1 || idempotency != 1 {
		t.Fatalf("parallel same-key create DB counts: machine=%d token=%d idempotency=%d，應各一",
			machines, tokens, idempotency)
	}
}

func TestEnrollCreateReplayRedactsSecretAndOffersRecovery(t *testing.T) {
	s, st := newServer(t)
	fresh, form := createEnrollment(t, s, "onode-replay", "replace lab host")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh create = %d：%s", fresh.Code, fresh.Body.String())
	}
	match := regexp.MustCompile(`(?s)<pre class="copy">([A-Za-z0-9_\-]{16,})</pre>`).
		FindStringSubmatch(fresh.Body.String())
	if match == nil {
		t.Fatalf("fresh response 沒有一次性 token：%s", fresh.Body.String())
	}
	token := match[1]

	// Browser refresh submits the exact frozen form. It must recover the
	// receipt, never mint a new key or reconstruct the original secret.
	replay := postForm(t, s, "/enrollments", form)
	if replay.Code != http.StatusOK {
		t.Fatalf("create replay = %d：%s", replay.Code, replay.Body.String())
	}
	if replay.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("redacted replay response 沒有 no-store：%q", replay.Header().Get("Cache-Control"))
	}
	replayBody := replay.Body.String()
	for _, want := range []string{"原票已建立", "token 無法重顯", "撤銷原票"} {
		if !strings.Contains(replayBody, want) {
			t.Errorf("recovery page 缺少 %q：\n%s", want, replayBody)
		}
	}
	if strings.Contains(replayBody, token) {
		t.Fatal("replay response 重顯了第一次 response 的 token")
	}
	if regexp.MustCompile(`(?s)<pre class="copy">[A-Za-z0-9_\-]{16,}</pre>`).MatchString(replayBody) {
		t.Fatal("replay recovery page 帶了任何像 enrollment secret 的 copy block")
	}

	entries, err := st.Audit("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].IsOperatorReplay() || entries[0].MachineID == "" ||
		entries[0].MachineID != entries[1].MachineID ||
		entries[0].IdempotencyKey != form.Get("idempotency_key") ||
		entries[0].RequestDigest != entries[1].RequestDigest {
		t.Fatalf("fresh/replay audit evidence 不對：%+v", entries)
	}
	if !strings.Contains(replayBody, `action="/machines/`+entries[0].MachineID+`/revoke-token/preview"`) {
		t.Fatalf("recovery page 沒有針對原 machine 的 revoke review：%s", replayBody)
	}
	ov, err := st.Overview(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if ov.Expected != 1 {
		t.Fatalf("replay 又建立了一台機器，分母=%d", ov.Expected)
	}
}

func TestEnrollCreateChangedReasonWithSameKeyConflicts(t *testing.T) {
	s, st := newServer(t)
	fresh, form := createEnrollment(t, s, "onode-conflict", "first reason")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh create = %d：%s", fresh.Code, fresh.Body.String())
	}
	changed := url.Values{}
	for key, values := range form {
		changed[key] = append([]string(nil), values...)
	}
	changed.Set("reason", "changed after review")
	conflict := postForm(t, s, "/enrollments", changed)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "idempotency") {
		t.Fatalf("same key with changed reason = %d：%s", conflict.Code, conflict.Body.String())
	}
	ov, err := st.Overview(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if ov.Expected != 1 {
		t.Fatalf("conflicting replay changed denominator to %d", ov.Expected)
	}
}

// TestMintedMachineIsInTheDenominatorImmediately
//
// ⚠ 開票就進分母，而且亮紅燈 —— 那是刻意的，不是 bug。
// 「我說要納管、但它沒來」必須看得見；一台等它出現才存在的機器，
// 就是 sampleagent3 隱形七週的那個 bug。
func TestMintedMachineIsInTheDenominatorImmediately(t *testing.T) {
	s, st := newServer(t)
	if rec, _ := createEnrollment(t, s, "sampleagent5", ""); rec.Code != http.StatusOK {
		t.Fatalf("開票 = %d：%s", rec.Code, rec.Body.String())
	}

	ov, err := st.Overview(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if ov.Expected != 1 {
		t.Fatalf("開票後分母是 %d，應該是 1", ov.Expected)
	}
	home := doGet(t, s, "/").Body.String()
	if !strings.Contains(home, "sampleagent5") {
		t.Error("開了票的機器沒有出現在首頁上")
	}
	// 而且那一頁要解釋那格紅燈是「還沒裝」，不是「壞了」。
	id := ov.Machines[0].MachineID
	page := doGet(t, s, "/machines/"+id).Body.String()
	if !strings.Contains(page, "開了票，還沒報到") {
		t.Errorf("機器頁沒有解釋那格紅燈的意思 —— 人會以為機器壞了")
	}
}

func TestMachinePagePendingEnrollmentUsesSafeOperatorProjection(t *testing.T) {
	s, st := newServer(t)
	machineID, token, err := st.CreateEnrollTokenFor("typed-pending", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var tokenHash string
	if err := st.DB().QueryRow(`SELECT token_hash FROM enrollment_tokens WHERE used_by=?`, machineID).
		Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+machineID)
	if !strings.Contains(body, "開了票，還沒報到") ||
		!strings.Contains(body, `action="/machines/`+machineID+`/revoke-token/preview"`) {
		t.Fatalf("pending enrollment metadata/action missing: %s", body)
	}
	if strings.Contains(body, token) || strings.Contains(body, tokenHash) {
		t.Fatal("machine page exposed enrollment token plaintext or hash")
	}

	createdAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	expiresAt := createdAt.Add(time.Hour)
	if _, err := st.DB().Exec(`UPDATE enrollment_tokens SET created_at=?,expires_at=? WHERE used_by=?`,
		createdAt.Format(time.RFC3339), expiresAt.Format(time.RFC3339), machineID); err != nil {
		t.Fatal(err)
	}
	expiredBody := get(t, s, "/machines/"+machineID)
	if !strings.Contains(expiredBody, "開過票，但它過期了") ||
		strings.Contains(expiredBody, token) || strings.Contains(expiredBody, tokenHash) {
		t.Fatalf("expired pending enrollment projection is missing or unsafe: %s", expiredBody)
	}
}

func TestMachinePagePendingEnrollmentAmbiguityFailsClosed(t *testing.T) {
	s, st := newServer(t)
	machineID, _, err := st.CreateEnrollTokenFor("ambiguous-pending", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	forgedHash := strings.Repeat("a", 64)
	if _, err := st.DB().Exec(`INSERT INTO enrollment_tokens
	 (token_hash,display_name,created_at,expires_at,used_by) VALUES (?,?,?,?,?)`,
		forgedHash, "ambiguous-pending", now.Format(time.RFC3339),
		now.Add(time.Hour).Format(time.RFC3339), machineID); err != nil {
		t.Fatal(err)
	}

	rec := doGet(t, s, "/machines/"+machineID)
	if rec.Code != http.StatusInternalServerError ||
		!strings.Contains(rec.Body.String(), "讀取 pending enrollment ticket 失敗") ||
		strings.Contains(rec.Body.String(), forgedHash) || strings.Contains(rec.Body.String(), "ambiguous pending enrollment token identity") {
		t.Fatalf("ambiguous pending identity did not fail closed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRevokedTokenCannotBeRedeemed 是「發票」那個動作的反向操作。
//
// ⚠ 這個 console 對自己立的規則是「每一個寫入都有反向操作，而且畫面上
// 看得到」。發票原本沒有 —— 票發出去就一直有效到過期，人只能等。
func TestRevokedTokenCannotBeRedeemed(t *testing.T) {
	s, st := newServer(t)
	rec, _ := createEnrollment(t, s, "sampleagent5", "")
	m := regexp.MustCompile(`(?s)<pre class="copy">([A-Za-z0-9_\-]{16,})</pre>`).
		FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatal("撈不到 token")
	}
	tok := m[1]

	entries, _ := st.Audit("", 10)
	id := entries[0].MachineID

	// 撤銷之前：兌換得了（用一個不會真的寫進去的方式確認票是有效的 ——
	// 這裡就直接兌換，因為下面要驗的是「撤銷之後不行」）。
	if p, err := st.PendingEnrollToken(id, time.Now().UTC()); err != nil || p == nil {
		t.Fatalf("撤銷前應該有一張待用的票：%v %+v", err, p)
	}

	preview, revokeForm := previewEnrollTokenRevocationForm(t, s, id, "名字打錯了")
	previewBody := preview.Body.String()
	if key := revokeForm.Get("idempotency_key"); !strings.HasPrefix(key, "web-enroll-token-revoke-") {
		t.Fatalf("revocation review request key prefix = %q", key)
	}
	if digest := revokeForm.Get("preview_digest"); !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("revocation review digest = %q", digest)
	}
	for _, want := range []string{
		"Pending enrollment ticket 將失效",
		"名字打錯了",
		"名冊列", "保留",
		"管理分母變化", `<span class="mono">0</span>`,
		"已啟用 agent credential", "不受影響",
		"票到期時間",
		`action="/machines/` + id + `/revoke-token"`,
	} {
		if !strings.Contains(previewBody, want) {
			t.Errorf("revocation review 缺少 %q：\n%s", want, previewBody)
		}
	}
	if preview.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("帶 request key 的 revocation review 沒有 no-store：%q", preview.Header().Get("Cache-Control"))
	}
	previewNav := renderedSubNavigation(t, previewBody, "裝置詳細資料子選單")
	if strings.Count(previewNav, `aria-current="location"`) != 1 ||
		!strings.Contains(previewNav, `aria-current="location">動作</a>`) ||
		!strings.Contains(previewNav, `href="/machines/`+id+`?section=machine-overview#machine-overview"`) {
		t.Fatalf("revocation review 沒有保留可達的 machine submenu，或 current 不是動作：%s", previewNav)
	}
	// Preview is a pure read: the same exact pending ticket remains and no
	// revocation audit/idempotency row exists yet.
	if p, err := st.PendingEnrollToken(id, time.Now().UTC()); err != nil || p == nil {
		t.Fatalf("preview 改掉了 pending enrollment ticket：%v %+v", err, p)
	}
	if entries, err := st.Audit("", 10); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 || entries[0].Action != store.AuditEnrollToken {
		t.Fatalf("preview 寫了 revocation audit：%+v", entries)
	}
	var previewKeyRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		revokeForm.Get("idempotency_key")).Scan(&previewKeyRows); err != nil || previewKeyRows != 0 {
		t.Fatalf("preview persisted request key: rows=%d err=%v", previewKeyRows, err)
	}

	if rec := postForm(t, s, "/machines/"+id+"/revoke-token", revokeForm); rec.Code != http.StatusOK {
		t.Fatalf("撤銷失敗：%d %s", rec.Code, rec.Body.String())
	} else {
		for _, want := range []string{
			"已撤銷 pending enrollment ticket", "revoked_at=", "token_was_expired=false",
			"名字打錯了",
			"名冊列保留", "管理分母變化 0", "active agent credential 未受影響",
			"此結果已留存",
		} {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("fresh revocation receipt missing %q: %s", want, rec.Body.String())
			}
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("fresh revocation receipt cache-control=%q", rec.Header().Get("Cache-Control"))
		}
	}
	if p, err := st.PendingEnrollToken(id, time.Now().UTC()); err != nil || p != nil {
		t.Errorf("撤銷後還有待用的票：%v %+v", err, p)
	}
	entries, err := st.Audit("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Action != store.AuditRevokeToken || !entries[0].OK ||
		entries[0].MachineID != id || entries[0].Subject != "sampleagent5" ||
		entries[0].Reason != "名字打錯了" || entries[0].IdempotencyKey != revokeForm.Get("idempotency_key") ||
		!strings.HasPrefix(entries[0].RequestDigest, "sha256:") ||
		entries[0].SourceKind != operator.SourceKindWeb ||
		!strings.Contains(entries[0].Detail, "registry_retained=true denominator_delta=0 active_agent_credential_affected=false") {
		t.Fatalf("fresh atomic revocation audit/receipt evidence 不完整：%+v", entries)
	}

	// Reposting the exact reviewed form is a successful replay, even though the
	// ticket row is already gone. It must not attempt a second direct deletion.
	if replay := postForm(t, s, "/machines/"+id+"/revoke-token", revokeForm); replay.Code != http.StatusOK ||
		!strings.Contains(replay.Body.String(), "撤銷結果（replay）") ||
		!strings.Contains(replay.Body.String(), "active agent credential 未受影響") {
		t.Fatalf("revocation replay = %d：%s", replay.Code, replay.Body.String())
	}
	replayEntries, err := st.Audit("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayEntries) != 3 || !replayEntries[0].IsOperatorReplay() ||
		replayEntries[0].IdempotencyKey != revokeForm.Get("idempotency_key") ||
		replayEntries[0].RequestDigest != entries[0].RequestDigest {
		t.Fatalf("revocation replay evidence 不對：%+v", replayEntries)
	}

	changed := url.Values{}
	for key, values := range revokeForm {
		changed[key] = append([]string(nil), values...)
	}
	changed.Set("reason", "review 後換了理由")
	conflict := postForm(t, s, "/machines/"+id+"/revoke-token", changed)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "idempotency") {
		t.Fatalf("same revocation key with changed reason = %d：%s", conflict.Code, conflict.Body.String())
	}
	if p, err := st.PendingEnrollToken(id, time.Now().UTC()); err != nil || p != nil {
		t.Fatalf("conflicting replay changed revoked state：%v %+v", err, p)
	}
	// ⚠ 真的去兌換一次 —— 那才是「不能再兌換」的意思。
	if _, _, err := st.RedeemEnrollToken(tok, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: "sampleagent5", OS: "linux", Arch: "amd64", UnixUser: "example-user",
	}, time.Now().UTC()); !errors.Is(err, store.ErrEnrollToken) {
		t.Errorf("撤銷過的票竟然還兌換得了：%v", err)
	}

	// ⚠ 撤銷**不會**把機器移出名冊。開票那一刻的宣告是事實，撤票不會
	// 讓它變成沒發生過 —— 要離開分母只有明確退役一條路。
	if ov, err := st.Overview(time.Now().UTC()); err != nil {
		t.Fatal(err)
	} else if ov.Expected != 1 {
		t.Errorf("撤票把機器一起移出分母了（分母 %d）—— 那是兩件事", ov.Expected)
	}
}

func TestEnrollTokenRevocationCraftedConfirmationsFailClosedAndKeepMachineNavigation(t *testing.T) {
	s, st := newServer(t)
	firstID, _, err := st.CreateEnrollTokenFor("crafted-revoke-one", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := st.CreateEnrollTokenFor("crafted-revoke-two", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, reviewed := previewEnrollTokenRevocationForm(t, s, firstID, "reviewed reason")

	clone := func(values url.Values) url.Values {
		out := url.Values{}
		for key, entries := range values {
			out[key] = append([]string(nil), entries...)
		}
		return out
	}
	missingPreview := clone(reviewed)
	missingPreview.Set("preview_digest", "")
	missingPreview.Set("idempotency_key", "crafted-missing-preview")
	stalePreview := clone(reviewed)
	stalePreview.Set("preview_digest", "sha256:"+strings.Repeat("a", 64))
	stalePreview.Set("idempotency_key", "crafted-stale-preview")
	differentTarget := clone(reviewed)
	differentTarget.Set("idempotency_key", "crafted-other-target")
	missingKey := clone(reviewed)
	missingKey.Set("idempotency_key", "")
	tests := []struct {
		name, path, wantText string
		status               int
		form                 url.Values
	}{
		{"missing preview", "/machines/" + firstID + "/revoke-token", "preview_digest 不可省略", http.StatusPreconditionRequired, missingPreview},
		{"stale preview", "/machines/" + firstID + "/revoke-token", "請重新預覽", http.StatusPreconditionFailed, stalePreview},
		{"different target", "/machines/" + secondID + "/revoke-token", "請重新預覽", http.StatusPreconditionFailed, differentTarget},
		{"missing key", "/machines/" + firstID + "/revoke-token", "Idempotency-Key 不可省略", http.StatusBadRequest, missingKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postForm(t, s, tc.path, tc.form)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantText) {
				t.Fatalf("crafted confirmation status=%d body=%s", rec.Code, rec.Body.String())
			}
			nav := renderedSubNavigation(t, rec.Body.String(), "裝置詳細資料子選單")
			if !strings.Contains(nav, `aria-current="location">動作</a>`) {
				t.Fatalf("crafted confirmation error lost machine action navigation: %s", nav)
			}
		})
	}
	for _, id := range []string{firstID, secondID} {
		if pending, err := st.PendingEnrollToken(id, time.Now().UTC()); err != nil || pending == nil {
			t.Fatalf("crafted confirmation mutated pending ticket %s: %+v err=%v", id, pending, err)
		}
	}
	var missingKeyRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=''`).Scan(&missingKeyRows); err != nil || missingKeyRows != 0 {
		t.Fatalf("missing idempotency key occupied ledger: rows=%d err=%v", missingKeyRows, err)
	}
}

func TestEnrollTokenRevocationRollsBackTicketDeleteWhenAtomicAuditFails(t *testing.T) {
	s, st := newServer(t)
	created, _ := createEnrollment(t, s, "onode-revoke-rollback", "")
	if created.Code != http.StatusOK {
		t.Fatalf("create = %d：%s", created.Code, created.Body.String())
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("create audit：err=%v entries=%+v", err, entries)
	}
	id := entries[0].MachineID
	_, revokeForm := previewEnrollTokenRevocationForm(t, s, id, "atomic evidence test")

	if _, err := st.DB().Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	failed := postForm(t, s, "/machines/"+id+"/revoke-token", revokeForm)
	if failed.Code != http.StatusInternalServerError ||
		!strings.Contains(failed.Body.String(), "控制面操作失敗") {
		t.Fatalf("audit write failure = %d：%s", failed.Code, failed.Body.String())
	}
	if pending, err := st.PendingEnrollToken(id, time.Now().UTC()); err != nil || pending == nil {
		t.Fatalf("audit failure 沒有 rollback exact ticket delete：err=%v pending=%+v", err, pending)
	}
	var cached int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		revokeForm.Get("idempotency_key")).Scan(&cached); err != nil || cached != 0 {
		t.Fatalf("rolled-back revocation 留下 cache：rows=%d err=%v", cached, err)
	}
}

// TestRetiredMachineCannotBeEnrolled
//
// ⚠ 這是一個真的會咬人的洞：票綁的機器退役之後，兌換照樣成功，
// 拿得到 agent token —— 然後之後每一個請求都 401，因為
// AuthenticateAgent 濾掉 retired 的機器。
//
// **「裝好了但一直 401」比「一開始就被拒絕」難查得多**，
// 而且 log 上看起來像憑證壞掉，會把人帶去查完全錯的方向。
func TestRetiredMachineCannotBeEnrolled(t *testing.T) {
	s, st := newServer(t)
	rec, _ := createEnrollment(t, s, "sampleagent5", "")
	m := regexp.MustCompile(`(?s)<pre class="copy">([A-Za-z0-9_\-]{16,})</pre>`).
		FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatal("撈不到 token")
	}
	tok := m[1]
	entries, _ := st.Audit("", 10)

	if err := st.RetireMachine(entries[0].MachineID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, _, err := st.RedeemEnrollToken(tok, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: "sampleagent5", OS: "linux", Arch: "amd64", UnixUser: "example-user",
	}, time.Now().UTC())
	if !errors.Is(err, store.ErrTokenRetired) {
		t.Errorf("退役的機器竟然報到成功了（err=%v）—— "+
			"那會拿到一個之後每次都 401 的身分", err)
	}
	// ⚠ 而且要 errors.Is 得到 ErrEnrollToken：HTTP 層靠它回同一個錯誤碼，
	// 不把「不存在 / 用過 / 過期 / 退役」的差別告訴還沒認證的呼叫端。
	if !errors.Is(err, store.ErrEnrollToken) {
		t.Errorf("ErrTokenRetired 沒有包在 ErrEnrollToken 底下：%v", err)
	}
}

// TestEnrollFormRefusesGET —— 跟其他寫入路徑同一條規則。
func TestEnrollFormRefusesGET(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	for _, path := range []string{
		"/enrollments/preview", "/enrollments",
		"/machines/" + id + "/revoke-token/preview",
		"/machines/" + id + "/revoke-token",
	} {
		if rec := doGet(t, s, path); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d，應該是 405", path, rec.Code)
		}
	}
}

// TestEnrollCommandUsesConfiguredHubBaseNotCallerHost
//
// HTTP Host 是 caller-controlled bytes，不可以滲入帶 token 的 agent 指令。
// 指令要跟早報共用啟動時釘住的 public/tailnet base。
func TestEnrollCommandUsesConfiguredHubBaseNotCallerHost(t *testing.T) {
	s, _ := newServer(t)
	s.SetHubBase("http://100.64.200.2:8787")

	mux := http.NewServeMux()
	s.Routes(mux)
	post := func(path, host string, form url.Values) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "100.64.200.2:54321"
		req.Host = host
		req = verifiedWebRequest(req, "example.com/cap/clawctl-admin")
		mux.ServeHTTP(rec, req)
		return rec
	}

	host := "attacker-controlled.invalid"
	preview := post("/enrollments/preview", host, url.Values{"name": {"sampleagent5"}})
	if preview.Code != http.StatusOK {
		t.Fatalf("preview = %d：%s", preview.Code, preview.Body.String())
	}
	createForm := url.Values{
		"name":            {"sampleagent5"},
		"preview_digest":  {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"idempotency_key": {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
	}
	created := post("/enrollments", host, createForm)
	if created.Code != http.StatusOK {
		t.Fatalf("create = %d：%s", created.Code, created.Body.String())
	}
	body := created.Body.String()
	if !strings.Contains(body, "--hub http://100.64.200.2:8787") {
		t.Errorf("指令裡沒有用已設定的 Hub base：\n%s", body)
	}
	if strings.Contains(body, "attacker-controlled.invalid") {
		t.Errorf("caller Host 滲入一次性 enroll 指令：\n%s", body)
	}
}
