package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestDiagnosticsWebPreviewApplyAndAccess(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "diagnostic-web")
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}

	viewOnly := renderWithCapabilities(t, s, "/machines/diagnostics", operatorauth.CapabilityNames{View: names.View})
	if !strings.Contains(viewOnly, "<h1>診斷</h1>") || !strings.Contains(viewOnly, "diagnostic-web") {
		t.Fatalf("view page=%s", viewOnly)
	}
	for _, forbidden := range []string{"diagnostic-noop-preview", "capability", "暫時", "temporary"} {
		if strings.Contains(viewOnly, forbidden) {
			t.Fatalf("view-only diagnostics exposed %q", forbidden)
		}
	}
	operate := renderWithCapabilities(t, s, "/machines/diagnostics", operatorauth.CapabilityNames{
		View: names.View, Operate: names.Operate,
	})
	if !strings.Contains(operate, `action="/machines/`+id+`/diagnostic-noop-preview"`) {
		t.Fatalf("operate page=%s", operate)
	}

	preview := postForm(t, s, "/machines/"+id+"/diagnostic-noop-preview", url.Values{
		"timeout": {"90"}, "reason": {"verify protocol"},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "建立 1 張 noop 工作單；機器設定不變。") ||
		!strings.Contains(preview.Body.String(), "<td>已啟用</td>") ||
		!strings.Contains(preview.Body.String(), `action="/machines/`+id+`/diagnostic-noop-jobs"`) {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	form := url.Values{
		"timeout":         {"90"},
		"confirm":         {"diagnostic-web"},
		"reason":          {"verify protocol"},
		"preview_digest":  {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"idempotency_key": {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
	}
	created := postForm(t, s, "/machines/"+id+"/diagnostic-noop-jobs", form)
	if created.Code != http.StatusSeeOther || !strings.HasPrefix(created.Header().Get("Location"), "/jobs/") {
		t.Fatalf("apply status=%d headers=%v body=%s", created.Code, created.Header(), created.Body.String())
	}
	if got := countWebRows(t, st, "desired_state"); got != 1 {
		t.Fatalf("desired_state=%d", got)
	}
	if got := countWebRows(t, st, "jobs"); got != 1 {
		t.Fatalf("jobs=%d", got)
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != store.AuditDiagnosticNoop ||
		entries[0].AuthCapability != names.Operate || !entries[0].OK {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}

	replay := postForm(t, s, "/machines/"+id+"/diagnostic-noop-jobs", form)
	if replay.Code != http.StatusSeeOther || replay.Header().Get("Location") != created.Header().Get("Location") ||
		countWebRows(t, st, "jobs") != 1 {
		t.Fatalf("replay status=%d headers=%v", replay.Code, replay.Header())
	}
}

func countWebRows(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestDiagnosticsWebBlocksDisabledExecution(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "diagnostic-disabled")
	disabled := false
	latest := checkin(time.Now().UTC().Add(time.Second))
	latest.JobsEnabled = &disabled
	if err := st.RecordCheckin(id, latest, latest.SentAt); err != nil {
		t.Fatal(err)
	}
	preview := postForm(t, s, "/machines/"+id+"/diagnostic-noop-preview", url.Values{
		"timeout": {"90"}, "reason": {"verify protocol"},
	})
	body := preview.Body.String()
	if preview.Code != http.StatusOK || !strings.Contains(body, "工作單執行未啟用") ||
		!strings.Contains(body, "jobs_enabled 設為 true") || strings.Contains(body, "建立診斷工作單</button>") {
		t.Fatalf("preview status=%d body=%s", preview.Code, body)
	}
}
