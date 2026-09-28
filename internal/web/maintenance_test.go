package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMaintenanceShowsCurrentPolicyAndUsesExactAdminCapability(t *testing.T) {
	s, _ := newServer(t)
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	view := renderWithCapabilities(t, s, "/tenant/maintenance", operatorauth.CapabilityNames{View: names.View})
	for _, want := range []string{"<h1>維護</h1>", "30 天", "14 天", "400 天", "尚未執行過清理", "租用戶管理"} {
		if !strings.Contains(view, want) {
			t.Errorf("view-only maintenance missing %q", want)
		}
	}
	if strings.Contains(view, `action="/tenant/maintenance/retention/prune-preview"`) {
		t.Fatal("view-only operator received retention mutation control")
	}
	admin := renderWithCapabilities(t, s, "/tenant/maintenance", operatorauth.CapabilityNames{Admin: names.Admin})
	if !strings.Contains(admin, `action="/tenant/maintenance/retention/prune-preview"`) ||
		!strings.Contains(admin, "預覽清理範圍") ||
		!strings.Contains(admin, `action="/tenant/maintenance/restore-drill-preview"`) {
		t.Fatal("admin maintenance page does not expose the delivered preview workflow")
	}
}

func maintenanceRestoreDrillBackup(t *testing.T, directory string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "backup-source.sqlite")
	backupStore, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	token, err := backupStore.CreateEnrollToken("restored-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := backupStore.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token, Hostname: "restored-machine",
		OS: "linux", Arch: "amd64", UnixUser: "operator",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := backupStore.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "clawctl-20260911T120000Z-before-web.sqlite"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceRestoreDrillPreviewEnqueueAndResult(t *testing.T) {
	s, st := newServer(t)
	directory := filepath.Join(t.TempDir(), "backups")
	maintenanceRestoreDrillBackup(t, directory)
	stamp := filepath.Join(t.TempDir(), "restore-drill.stamp")
	service := operator.New(st)
	service.ConfigureRestoreDrill(restoredrill.Runner{BackupsDir: directory, StampPath: stamp, Live: st})
	s.SetOperatorService(service)
	s.SetDrillStampReader(func() (time.Time, bool) { return restoredrill.ReadStamp(stamp) })

	preview := postForm(t, s, "/tenant/maintenance/restore-drill-preview", url.Values{
		"reason": {"季度備份復原驗證"},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "確認還原演練") ||
		!strings.Contains(preview.Body.String(), "VERIFY clawctl-20260911T120000Z-before-web.sqlite") ||
		!strings.Contains(preview.Body.String(), "正式資料庫不會被替換") ||
		!strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("preview=%d body=%s", preview.Code, preview.Body.String())
	}
	form := url.Values{
		"preview_digest":  {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"reason":          {hiddenFormValue(t, preview.Body.String(), "reason")},
		"idempotency_key": {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
		"confirm":         {"VERIFY clawctl-20260911T120000Z-before-web.sqlite"},
	}
	created := postForm(t, s, "/tenant/maintenance/restore-drills", form)
	if created.Code != http.StatusSeeOther || !strings.HasPrefix(created.Header().Get("Location"), "/tenant/maintenance/restore-drills/") {
		t.Fatalf("created=%d headers=%v body=%s", created.Code, created.Header(), created.Body.String())
	}
	operationPath := created.Header().Get("Location")
	queued := tailnetAdminGet(t, s, operationPath)
	if queued.Code != http.StatusOK || !strings.Contains(queued.Body.String(), "已排隊") ||
		!strings.Contains(queued.Body.String(), "重新整理") {
		t.Fatalf("queued=%d body=%s", queued.Code, queued.Body.String())
	}
	if processed, err := service.RunQueuedRestoreDrillOperations(t.Context()); err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	completed := tailnetAdminGet(t, s, operationPath)
	if completed.Code != http.StatusOK || !strings.Contains(completed.Body.String(), "備份可還原") ||
		!strings.Contains(completed.Body.String(), "成功") || !strings.Contains(completed.Body.String(), "</html>") {
		t.Fatalf("completed=%d body=%s", completed.Code, completed.Body.String())
	}
	maintenance := tailnetAdminGet(t, s, "/tenant/maintenance")
	if maintenance.Code != http.StatusOK || !strings.Contains(maintenance.Body.String(), "最近的還原演練") ||
		!strings.Contains(maintenance.Body.String(), "90 天內已完成") || !strings.Contains(maintenance.Body.String(), operationPath) {
		t.Fatalf("maintenance=%d body=%s", maintenance.Code, maintenance.Body.String())
	}
	entries, err := st.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditRestoreDrill}, Limit: 10})
	if err != nil || len(entries.Items) != 1 || entries.Items[0].SourceKind != "web" ||
		entries.Items[0].Outcome == nil || *entries.Items[0].Outcome != store.AuditOutcomeOK {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}

func TestMaintenancePrunePreviewApplyAndReplay(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "maintenance-machine")
	now := time.Now().UTC()
	for _, at := range []time.Time{now.Add(-20 * 24 * time.Hour), now.Add(-18 * 24 * time.Hour)} {
		if err := st.RecordCheckin(id, checkin(at), at); err != nil {
			t.Fatalf("old checkin: %v", err)
		}
	}

	preview := postForm(t, s, "/tenant/maintenance/retention/prune-preview", url.Values{
		"reason": {"例行資料保留維護"},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "確認永久清理") ||
		!strings.Contains(preview.Body.String(), "DELETE 2 ROWS") ||
		!strings.Contains(preview.Body.String(), "保護的最新證據") ||
		!strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("preview=%d body=%s", preview.Code, preview.Body.String())
	}
	form := url.Values{
		"evaluated_at":         {hiddenFormValue(t, preview.Body.String(), "evaluated_at")},
		"observations_seconds": {hiddenFormValue(t, preview.Body.String(), "observations_seconds")},
		"checkins_seconds":     {hiddenFormValue(t, preview.Body.String(), "checkins_seconds")},
		"occupancy_seconds":    {hiddenFormValue(t, preview.Body.String(), "occupancy_seconds")},
		"expected_revision":    {hiddenFormValue(t, preview.Body.String(), "expected_revision")},
		"preview_digest":       {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"reason":               {hiddenFormValue(t, preview.Body.String(), "reason")},
		"idempotency_key":      {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
		"confirm":              {"DELETE 2 ROWS"},
	}
	applied := postForm(t, s, "/tenant/maintenance/retention/prunes", form)
	if applied.Code != http.StatusOK || !strings.Contains(applied.Body.String(), "已永久刪除 2 列") ||
		!strings.Contains(applied.Body.String(), "此結果已留存") {
		t.Fatalf("apply=%d body=%s", applied.Code, applied.Body.String())
	}
	var oldRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM machine_checkins WHERE machine_id=? AND received_at<?`,
		id, now.Add(-14*24*time.Hour).Format(time.RFC3339Nano)).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatalf("old checkins remain=%d err=%v", oldRows, err)
	}

	replayed := postForm(t, s, "/tenant/maintenance/retention/prunes", form)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), "沒有再次刪除資料") {
		t.Fatalf("replay=%d body=%s", replayed.Code, replayed.Body.String())
	}
	entries, err := st.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditRetentionPrune}, Limit: 10,
	})
	if err != nil || len(entries.Items) != 2 || entries.Items[0].SourceKind != "web" ||
		entries.Items[0].IdempotencyKey != form.Get("idempotency_key") {
		t.Fatalf("retention audit=%+v err=%v", entries, err)
	}
	status := tailnetAdminGet(t, s, "/tenant/maintenance")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), "刪除 2 列") {
		t.Fatalf("maintenance status=%d body=%s", status.Code, status.Body.String())
	}
}

func TestMaintenanceZeroRowPreviewHasNoApplyAction(t *testing.T) {
	s, _ := newServer(t)
	preview := postForm(t, s, "/tenant/maintenance/retention/prune-preview", url.Values{
		"reason": {"確認目前資料保留範圍"},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "目前沒有資料需要清理") {
		t.Fatalf("zero preview=%d body=%s", preview.Code, preview.Body.String())
	}
	if strings.Contains(preview.Body.String(), `action="/tenant/maintenance/retention/prunes"`) ||
		strings.Contains(preview.Body.String(), `name="idempotency_key"`) {
		t.Fatal("zero-row preview minted an apply action")
	}
}

func TestMaintenanceMalformedApplyIsAuditedWithoutReceipt(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
		code operator.RetentionTransportRejectionCode
	}{
		{"invalid evaluated at", url.Values{
			"evaluated_at": {"not-a-time"}, "idempotency_key": {"malformed-web-retention-time"},
		}, operator.RetentionTransportRejectionEvaluatedAtInvalid},
		{"evaluated at is not a whole second", url.Values{
			"evaluated_at":    {"2026-09-14T00:00:00.5Z"},
			"idempotency_key": {"malformed-web-retention-fraction"},
		}, operator.RetentionTransportRejectionEvaluatedAtInvalid},
		{"invalid coordinates", url.Values{
			"evaluated_at": {"2026-09-14T00:00:00Z"}, "observations_seconds": {"invalid"},
			"checkins_seconds": {"1"}, "occupancy_seconds": {"1"}, "expected_revision": {"0"},
			"idempotency_key": {"malformed-web-retention-coordinates"},
		}, operator.RetentionTransportRejectionCoordinatesInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, st := newServer(t)
			response := postForm(t, s, "/tenant/maintenance/retention/prunes", test.form)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "請重新預覽") {
				t.Fatalf("malformed apply=%d body=%s", response.Code, response.Body.String())
			}
			var receipts int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
				test.form.Get("idempotency_key")).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatalf("transport rejection receipt count=%d err=%v", receipts, err)
			}
			entries, err := st.Audit("", 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			wantDetail := store.OperatorTransportRejectionPrefix + string(test.code) +
				": canonical request digest 無法取得"
			if entry.Action != store.AuditRetentionPrune || entry.Subject != "retention" || entry.OK ||
				entry.Detail != wantDetail || entry.IdempotencyKey != test.form.Get("idempotency_key") ||
				entry.RequestDigest != "" || entry.Reason != "" || entry.SourceKind != operator.SourceKindWeb ||
				entry.AuthSubject != "tailscale-user:42" {
				t.Fatalf("transport audit shape=%+v", entry)
			}
		})
	}
}
