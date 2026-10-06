package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// 這個 console 的寫入路徑。
//
// 這些 handler 只由 Hub 的 operator boundary 掛載：每一個 request 都先經過
// Tailscale LocalAPI WhoIsForIP、該 route 的 exact app capability 與 Go 標準庫
// CrossOriginProtection。handler 只讀 context 中那次已驗證的 principal，不再
// 執行第二次 whois，也不相信 Forwarded/Tailscale-* 這類 caller headers。
//
// 所以三條規則，每一條都對應一種真的會發生的事：
//
//  1. **POST only。** GET 不准改任何東西。
//     瀏覽器、聊天軟體、iOS 的連結預覽都會替你 GET 一個網址 ——
//     一個 `GET /machines/x/retire` 會在你把連結貼進 Telegram 的那一刻執行。
//
//  2. **打字確認。** retire 要求把機器名字打對。
//     這不是為了防惡意，是為了讓「按錯」需要一個刻意的動作。
//
//  3. **可以反悔。** 每一個寫入都有反向操作，而且畫面上看得到。
//     retire 改的是**分母**，而「機器不准從名冊上消失」正好是這整個
//     產品存在的理由 —— 一個按下去就回不來的 retire 按鈕，是在這個
//     產品自己的核心承諾上開一個洞。
//
// ⚠ Tailscale 證明的是來源 node 與登入該 node 的 tailnet owner，不是 fresh
// MFA，也不證明此刻坐在鍵盤前的人。因此 POST-only、打字確認與可反悔仍然
// 有各自價值，不能因為有授權就拆掉。

func (s *Server) actionRoutes(mux *http.ServeMux) []string {
	patterns := []string{
		"GET /audit",
		"POST /machines/{id}/connect",
		"POST /machines/{id}/retire",
		"POST /machines/{id}/unretire",
		"POST /enrollments/preview",
		"POST /enrollments",
		"POST /machines/{id}/revoke-token/preview",
		"POST /machines/{id}/revoke-token",
		"POST /machines/{id}/channel",
		"POST /deployments/preview",
		"POST /deployments",
		"POST /deployments/{id}/continue-preview",
		"POST /deployments/{id}/continue",
		"POST /deployments/{id}/retry-preview",
		"POST /deployments/{id}/retry",
		"POST /deployments/{id}/abandon-preview",
		"POST /deployments/{id}/abandon",
		"POST /apps/artifact-fetches/preview",
		"POST /apps/artifact-fetches",
		"POST /apps/store/packages/preview",
		"POST /apps/store/packages",
		"POST /apps/profiles/preview",
		"POST /apps/profiles",
		"POST /apps/profile-assignments/preview",
		"POST /apps/profile-assignments",
		"POST /machines/{id}/lifecycle-preview",
		"POST /machines/configuration/policy-preview",
		"POST /machines/configuration/policies",
		"POST /machines/configuration/assignment-preview",
		"POST /machines/configuration/assignments",
		"POST /machines/compliance/policy-preview",
		"POST /machines/compliance/policies",
		"POST /machines/compliance/assignment-preview",
		"POST /machines/compliance/assignments",
		"POST /machines/{id}/diagnostic-noop-preview",
		"POST /machines/{id}/diagnostic-noop-jobs",
		"POST /jobs/{id}/verifier-assignment-preview",
		"POST /jobs/{id}/verifier-assignments",
		"POST /settings/tailnet/peer-ignore-preview",
		"POST /settings/tailnet/peer-ignores",
		"POST /tenant/maintenance/retention/prune-preview",
		"POST /tenant/maintenance/retention/prunes",
		"POST /tenant/maintenance/restore-drill-preview",
		"POST /tenant/maintenance/restore-drills",
		"POST /machines/enrollment/limit-preview",
		"POST /machines/enrollment/limits",
		"POST /machines/{id}/display-name-preview",
		"POST /machines/{id}/display-name",
		"POST /machines/{id}/notes-preview",
		"POST /machines/{id}/notes",
		"POST /deployments/{id}/skip-failed-batch-preview",
		"POST /deployments/{id}/skip-failed-batch",
	}
	mux.HandleFunc(patterns[0], s.auditPage)
	mux.HandleFunc(patterns[1], s.doConnect)
	mux.HandleFunc(patterns[2], s.doRetire)
	mux.HandleFunc(patterns[3], s.doUnretire)
	mux.HandleFunc(patterns[4], s.previewEnrollToken)
	mux.HandleFunc(patterns[5], s.doEnrollToken)
	mux.HandleFunc(patterns[6], s.previewRevokeToken)
	mux.HandleFunc(patterns[7], s.doRevokeToken)
	mux.HandleFunc(patterns[8], s.doMachineChannel)
	mux.HandleFunc(patterns[9], s.previewDeploymentCreate)
	mux.HandleFunc(patterns[10], s.applyDeploymentCreate)
	mux.HandleFunc(patterns[11], s.previewDeploymentContinue)
	mux.HandleFunc(patterns[12], s.applyDeploymentContinue)
	mux.HandleFunc(patterns[13], s.previewDeploymentRetry)
	mux.HandleFunc(patterns[14], s.applyDeploymentRetry)
	mux.HandleFunc(patterns[15], s.previewDeploymentAbandon)
	mux.HandleFunc(patterns[16], s.applyDeploymentAbandon)
	mux.HandleFunc(patterns[17], s.previewArtifactFetch)
	mux.HandleFunc(patterns[18], s.applyArtifactFetch)
	mux.HandleFunc(patterns[19], s.previewCatalogPackage)
	mux.HandleFunc(patterns[20], s.publishCatalogPackage)
	mux.HandleFunc(patterns[21], s.previewCatalogProfile)
	mux.HandleFunc(patterns[22], s.publishCatalogProfile)
	mux.HandleFunc(patterns[23], s.previewCatalogAssignment)
	mux.HandleFunc(patterns[24], s.applyCatalogAssignment)
	mux.HandleFunc(patterns[25], s.previewMachineLifecycle)
	mux.HandleFunc(patterns[26], s.previewSettingPolicy)
	mux.HandleFunc(patterns[27], s.publishSettingPolicy)
	mux.HandleFunc(patterns[28], s.previewSettingAssignment)
	mux.HandleFunc(patterns[29], s.applySettingAssignment)
	mux.HandleFunc(patterns[30], s.previewCompliancePolicy)
	mux.HandleFunc(patterns[31], s.publishCompliancePolicy)
	mux.HandleFunc(patterns[32], s.previewComplianceAssignment)
	mux.HandleFunc(patterns[33], s.applyComplianceAssignment)
	mux.HandleFunc(patterns[34], s.previewDiagnosticNoop)
	mux.HandleFunc(patterns[35], s.applyDiagnosticNoop)
	mux.HandleFunc(patterns[36], s.previewVerifierAssignment)
	mux.HandleFunc(patterns[37], s.applyVerifierAssignment)
	mux.HandleFunc(patterns[38], s.previewTailnetPeerIgnore)
	mux.HandleFunc(patterns[39], s.applyTailnetPeerIgnore)
	mux.HandleFunc(patterns[40], s.previewRetentionPrune)
	mux.HandleFunc(patterns[41], s.applyRetentionPrune)
	mux.HandleFunc(patterns[42], s.previewRestoreDrill)
	mux.HandleFunc(patterns[43], s.applyRestoreDrill)
	mux.HandleFunc(patterns[44], s.previewEnrollmentLimit)
	mux.HandleFunc(patterns[45], s.applyEnrollmentLimit)
	mux.HandleFunc(patterns[46], s.previewMachineRename)
	mux.HandleFunc(patterns[47], s.applyMachineRename)
	mux.HandleFunc(patterns[48], s.previewMachineNotes)
	mux.HandleFunc(patterns[49], s.applyMachineNotes)
	mux.HandleFunc(patterns[50], s.previewDeploymentSkipFailedBatch)
	mux.HandleFunc(patterns[51], s.applyDeploymentSkipFailedBatch)
	return patterns
}

func (s *Server) previewMachineNotes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := r.FormValue("reason")
	if strings.TrimSpace(reason) == "" || reason != strings.TrimSpace(reason) || len(reason) > 500 {
		s.renderActionStatus(w, r, http.StatusBadRequest, id, "沒有建立名冊備註預覽",
			"理由不可省略、前後不可有空白，且最多 500 bytes。", "/machines/"+id)
		return
	}
	preview, err := s.operator.PreviewMachineNotes(operator.MachineNotesPreviewRequest{
		MachineID: id, Notes: r.FormValue("notes"),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if errors.Is(err, store.ErrNotFound) {
			status, detail = http.StatusNotFound, "找不到這台機器。"
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator machine notes preview 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id, "沒有建立名冊備註預覽", detail, "/machines/"+id)
		return
	}
	key, err := operator.NewIdempotencyKey("web-machine-notes")
	if err != nil {
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.DisplayName,
			"沒有建立名冊備註預覽", "無法建立 request key。", "/machines/"+id)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "machine_notes_review.html", page{
		Title: "確認名冊備註 " + preview.DisplayName, Nav: "machines-detail",
		Now: time.Now().Local().Format("2006-01-02 15:04"), MachineNotesPreview: &preview,
		MachineNotesReason: reason, IdempotencyKey: key,
	})
}

func (s *Server) applyMachineNotes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.operator.UpdateMachineNotes(operator.MachineNotesRequest{
		MachineID: id, Notes: r.FormValue("notes"),
		ConfirmDisplayName: r.FormValue("confirm_display_name"),
		PreviewDigest:      r.FormValue("preview_digest"), Reason: r.FormValue("reason"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator machine notes apply 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id, "沒有更新名冊備註", detail, "/machines/"+id)
		return
	}
	http.Redirect(w, r, "/machines/"+id+"?section=actions#actions", http.StatusSeeOther)
}

func (s *Server) previewMachineRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := r.FormValue("reason")
	if strings.TrimSpace(reason) == "" || reason != strings.TrimSpace(reason) || len(reason) > 500 {
		s.renderActionStatus(w, r, http.StatusBadRequest, id, "沒有建立重新命名預覽",
			"理由不可省略、前後不可有空白，且最多 500 bytes。", "/machines/"+id)
		return
	}
	preview, err := s.operator.PreviewMachineRename(operator.MachineRenamePreviewRequest{
		MachineID: id, DisplayName: r.FormValue("display_name"),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if errors.Is(err, store.ErrNotFound) {
			status, detail = http.StatusNotFound, "找不到這台機器。"
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator machine rename preview 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id, "沒有建立重新命名預覽", detail, "/machines/"+id)
		return
	}
	key, err := operator.NewIdempotencyKey("web-machine-rename")
	if err != nil {
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.CurrentDisplayName,
			"沒有建立重新命名預覽", "無法建立 request key。", "/machines/"+id)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "machine_rename_review.html", page{
		Title: "確認重新命名 " + preview.CurrentDisplayName, Nav: "machines-detail",
		Now: time.Now().Local().Format("2006-01-02 15:04"), MachineRenamePreview: &preview,
		MachineRenameReason: reason, IdempotencyKey: key,
	})
}

func (s *Server) applyMachineRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	result, err := s.operator.RenameMachine(operator.MachineRenameRequest{
		MachineID: id, DisplayName: r.FormValue("display_name"),
		ConfirmDisplayName: r.FormValue("confirm_display_name"),
		PreviewDigest:      r.FormValue("preview_digest"), Reason: r.FormValue("reason"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator machine rename apply 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id, "沒有重新命名", detail, "/machines/"+id)
		return
	}
	_ = result
	http.Redirect(w, r, "/machines/"+id+"?section=actions#actions", http.StatusSeeOther)
}

func (s *Server) previewRestoreDrill(w http.ResponseWriter, r *http.Request) {
	reason := r.FormValue("reason")
	if reason == "" || strings.TrimSpace(reason) != reason || len(reason) > 500 {
		s.renderActionStatus(w, r, http.StatusBadRequest, "還原演練", "未建立演練預覽",
			"理由不可省略、前後不可有空白，且最多 500 字。", "/tenant/maintenance")
		return
	}
	preview, err := s.operator.PreviewRestoreDrill(r.Context(), time.Now().UTC())
	if err != nil {
		log.Printf("operator restore drill preview failed: %v", err)
		s.renderActionStatus(w, r, http.StatusServiceUnavailable, "還原演練", "未建立演練預覽",
			"目前無法讀取可驗證的備份。", "/tenant/maintenance")
		return
	}
	key, err := operator.NewIdempotencyKey("web-restore-drill")
	if err != nil {
		s.renderActionStatus(w, r, http.StatusInternalServerError, "還原演練", "未建立演練預覽",
			"無法建立 request key。", "/tenant/maintenance")
		return
	}
	s.render(w, r, "maintenance_restore_drill_review.html", page{
		Title: "確認還原演練", Nav: "tenant-maintenance", Now: time.Now().Local().Format("2006-01-02 15:04"),
		RestoreDrillPreview: &preview, RestoreDrillReason: reason, IdempotencyKey: key,
	})
}

func (s *Server) applyRestoreDrill(w http.ResponseWriter, r *http.Request) {
	result, err := s.operator.ApplyRestoreDrill(r.Context(), operator.RestoreDrillApplyRequest{
		PreviewDigest: r.FormValue("preview_digest"), Confirm: r.FormValue("confirm"),
		Reason: r.FormValue("reason"), IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if errors.Is(err, operator.ErrRestoreDrillUnavailable) {
			status, detail = http.StatusServiceUnavailable, "還原演練目前無法使用。"
		}
		if status == http.StatusInternalServerError {
			status, detail = http.StatusServiceUnavailable, "還原演練目前無法建立。"
			log.Printf("operator restore drill apply failed: %v", err)
		}
		s.renderActionStatus(w, r, status, "還原演練", "未建立還原演練", detail, "/tenant/maintenance")
		return
	}
	http.Redirect(w, r, "/tenant/maintenance/restore-drills/"+result.Operation.OperationID, http.StatusSeeOther)
}

func (s *Server) previewRetentionPrune(w http.ResponseWriter, r *http.Request) {
	reason := r.FormValue("reason")
	if reason == "" || strings.TrimSpace(reason) != reason || len(reason) > 500 {
		s.renderActionStatus(w, r, http.StatusBadRequest, "資料保留政策",
			"未建立清理預覽", "理由不可省略、前後不可有空白，且最多 500 字。", "/tenant/maintenance")
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	preview, err := s.operator.PreviewRetentionPrune(operator.RetentionPrunePreviewRequest{
		EvaluatedAt: now, Policy: s.retentionPolicy,
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator retention preview failed: %v", err)
		}
		s.renderActionStatus(w, r, status, "資料保留政策", "未建立清理預覽", detail, "/tenant/maintenance")
		return
	}
	if preview.TotalDeleted == 0 {
		s.renderActionStatus(w, r, http.StatusOK, "資料保留政策", "目前沒有資料需要清理",
			fmt.Sprintf("已依目前保留政策檢查五張證據表；沒有可刪除列，並保護 %d 筆各組最新證據。", preview.KeptNewest),
			"/tenant/maintenance")
		return
	}
	key, err := operator.NewIdempotencyKey("web-retention-prune")
	if err != nil {
		s.renderActionStatus(w, r, http.StatusInternalServerError, "資料保留政策",
			"未建立清理預覽", "無法建立 request key。", "/tenant/maintenance")
		return
	}
	s.render(w, r, "maintenance_prune_review.html", page{
		Title: "確認永久清理", Nav: "tenant-maintenance", Now: now.Local().Format("2006-01-02 15:04"),
		RetentionPrunePreview: &preview, RetentionPruneReason: reason, IdempotencyKey: key,
	})
}

func (s *Server) applyRetentionPrune(w http.ResponseWriter, r *http.Request) {
	back := "/tenant/maintenance"
	evaluatedAt, err := time.Parse(time.RFC3339Nano, r.FormValue("evaluated_at"))
	if err != nil || !evaluatedAt.Equal(evaluatedAt.UTC().Truncate(time.Second)) {
		s.recordRetentionWebTransportRejection(r, operator.RetentionTransportRejectionEvaluatedAtInvalid)
		s.renderActionStatus(w, r, http.StatusBadRequest, "資料保留政策", "未清理資料", "評估時間不合法；請重新預覽。", back)
		return
	}
	parseCoordinate := func(name string) (int64, error) {
		return strconv.ParseInt(r.FormValue(name), 10, 64)
	}
	observations, obsErr := parseCoordinate("observations_seconds")
	checkins, checkErr := parseCoordinate("checkins_seconds")
	occupancy, occupancyErr := parseCoordinate("occupancy_seconds")
	revision, revisionErr := parseCoordinate("expected_revision")
	if obsErr != nil || checkErr != nil || occupancyErr != nil || revisionErr != nil || revision < 0 {
		s.recordRetentionWebTransportRejection(r, operator.RetentionTransportRejectionCoordinatesInvalid)
		s.renderActionStatus(w, r, http.StatusBadRequest, "資料保留政策", "未清理資料", "預覽座標不合法；請重新預覽。", back)
		return
	}
	result, err := s.operator.ApplyRetentionPrune(operator.RetentionPruneApplyRequest{
		EvaluatedAt: evaluatedAt,
		Policy: store.OperatorRetentionPolicy{
			ObservationsSeconds: observations, CheckinsSeconds: checkins, OccupancySeconds: occupancy,
		},
		ExpectedRevision: revision, Confirm: r.FormValue("confirm"),
		PreviewDigest: r.FormValue("preview_digest"), Reason: r.FormValue("reason"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator retention apply failed: %v", err)
		}
		s.renderActionStatus(w, r, status, "資料保留政策", "未清理資料", detail, back)
		return
	}
	detail := fmt.Sprintf("已永久刪除 %d 列，並保護 %d 筆各組最新證據；retention revision 現為 %d。",
		result.TotalDeleted, result.KeptNewest, result.Revision)
	if result.Replayed {
		detail = fmt.Sprintf("已回放原判決；沒有再次刪除資料。原判決刪除 %d 列，retention revision 為 %d。",
			result.TotalDeleted, result.Revision)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, r, "action.html", page{
		Title: "資料清理完成", Nav: "tenant-maintenance", Now: time.Now().Local().Format("2006-01-02 15:04"),
		Action: &actionResult{Subject: "資料保留政策", Headline: "資料清理完成", Detail: detail,
			Back: back, BackLabel: actionBackLabel(back), Changed: true},
	})
}

func (s *Server) recordRetentionWebTransportRejection(r *http.Request, code operator.RetentionTransportRejectionCode) {
	err := s.operator.RecordRetentionTransportRejection(operator.RetentionTransportRejectionRequest{
		Code: code, IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		log.Printf("operator retention Web transport audit failed code=%s: %v", code, err)
	}
}

func (s *Server) previewTailnetPeerIgnore(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	action := strings.TrimSpace(r.FormValue("action"))
	var expiresAt time.Time
	if action == "ignore" {
		days, err := strconv.Atoi(r.FormValue("days"))
		if err != nil || days < 1 || days > 366 {
			s.renderActionStatus(w, r, http.StatusBadRequest, r.FormValue("peer_id"),
				"未建立 Tailnet 規則預覽", "效期必須是 1 至 366 天。", "/settings/tailnet")
			return
		}
		expiresAt = now.Add(time.Duration(days) * 24 * time.Hour)
	}
	preview, err := s.operator.PreviewTailnetPeerIgnore(r.Context(), operator.TailnetPeerIgnorePreviewRequest{
		PeerID: r.FormValue("peer_id"), Action: action, ExpiresAt: expiresAt, Reason: r.FormValue("reason"),
	}, now)
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator tailnet preview failed: %v", err)
		}
		s.renderActionStatus(w, r, status, r.FormValue("peer_id"), "未建立 Tailnet 規則預覽", detail, "/settings/tailnet")
		return
	}
	key, err := operator.NewIdempotencyKey("web-tailnet-ignore")
	if err != nil {
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.Hostname,
			"未建立 Tailnet 規則預覽", "無法建立 request key。", "/settings/tailnet")
		return
	}
	s.render(w, r, "tailnet_ignore_review.html", page{
		Title: "確認 Tailnet 規則", Nav: "tailnet", Now: now.Local().Format("2006-01-02 15:04"),
		TailnetIgnorePreview: &preview, IdempotencyKey: key,
	})
}

func (s *Server) applyTailnetPeerIgnore(w http.ResponseWriter, r *http.Request) {
	expectedRevision, err := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	if err != nil || expectedRevision < 0 {
		s.renderActionStatus(w, r, http.StatusBadRequest, r.FormValue("peer_id"),
			"未變更 Tailnet 規則", "expected revision 不合法。", "/settings/tailnet")
		return
	}
	var expiresAt time.Time
	if raw := strings.TrimSpace(r.FormValue("expires_at")); raw != "" {
		expiresAt, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			s.renderActionStatus(w, r, http.StatusBadRequest, r.FormValue("peer_id"),
				"未變更 Tailnet 規則", "expires_at 不合法。", "/settings/tailnet")
			return
		}
	}
	result, err := s.operator.ApplyTailnetPeerIgnore(r.Context(), operator.TailnetPeerIgnoreApplyRequest{
		TailnetPeerIgnorePreviewRequest: operator.TailnetPeerIgnorePreviewRequest{
			PeerID: r.FormValue("peer_id"), Action: r.FormValue("action"),
			ExpiresAt: expiresAt, Reason: r.FormValue("reason"),
		},
		ExpectedRevision: expectedRevision, ConfirmHostname: r.FormValue("confirm_hostname"),
		PreviewDigest: r.FormValue("preview_digest"), IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	}, time.Now().UTC())
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator tailnet apply failed: %v", err)
		}
		s.renderActionStatus(w, r, status, r.FormValue("peer_id"), "未變更 Tailnet 規則", detail, "/settings/tailnet")
		return
	}
	_ = result
	http.Redirect(w, r, "/settings/tailnet", http.StatusSeeOther)
}

// doMachineChannel 改一台機器的可反悔通道指派。
func (s *Server) doMachineChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	channel := r.FormValue("channel")
	revision, parseErr := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	var expected *int64
	if parseErr == nil {
		expected = &revision
	}
	req := operator.MachineChannelRequest{
		MachineID: id, Channel: channel, ExpectedRevision: expected,
		ConfirmDisplayName: r.FormValue("confirm"),
		IdempotencyKey:     r.FormValue("idempotency_key"),
		Actor:              operator.ActorFromRequest(r, operator.SourceKindWeb),
	}
	if _, err := s.operator.ChangeMachineChannel(req); err != nil {
		status, _, detail := operator.HTTPError(err)
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			detail = "這是原 request 的回放判決，未重新評估目前狀態：" + detail
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator machine channel 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id,
			"沒有改 channel", detail, "/machines/"+id)
		return
	}
	http.Redirect(w, r, "/machines/"+id, http.StatusSeeOther)
}

func channelLabel(channel string) string {
	if channel == "" {
		return "未指派"
	}
	return channel
}

// enrollTTL 是網頁開出來的票的壽命。
//
// ⚠ 比 CLI 的 24h 短。理由是這兩個動作的情境不一樣：CLI 開票通常是
// 「等一下要去裝那台」，網頁開票是「我現在人就在這台前面」。
// 一張活 24 小時的票，在它被用掉之後還會在瀏覽器歷史裡躺一整天。
const enrollTTL = 2 * time.Hour

const enrollTTLSeconds = int64(enrollTTL / time.Second)

// previewEnrollToken is deliberately a separate POST from creation.  The
// first form has no secret and changes no state: it freezes the domain-owned
// impact digest and mints exactly one request key for the confirmation page.
// Refreshing or resubmitting the eventual create therefore cannot mint a
// second credential under a conveniently new key.
func (s *Server) previewEnrollToken(w http.ResponseWriter, r *http.Request) {
	preview, err := s.operator.PreviewEnrollToken(operator.EnrollTokenPreviewRequest{
		DisplayName: r.FormValue("name"),
		TTLSeconds:  enrollTTLSeconds,
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enroll preview 失敗: %v", err)
		}
		s.renderActionStatus(w, r, status, strings.TrimSpace(r.FormValue("name")),
			"沒有建立 enroll 預覽", detail, "/machines/enrollment")
		return
	}
	if err := validateEnrollPreviewRenderResult(preview); err != nil {
		log.Printf("operator enroll preview 回傳不一致：%v", err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.DisplayName,
			"沒有建立 enroll 預覽", "預覽結果不符合一次性憑證與名冊影響契約。", "/machines/enrollment")
		return
	}

	key, err := operator.NewIdempotencyKey("web-enroll-token")
	if err != nil {
		log.Printf("operator enroll preview 產生 idempotency key 失敗: %v", err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.DisplayName,
			"沒有建立 enroll 預覽", "無法產生這次確認所需的 request key。請回機器註冊後重試。", "/machines/enrollment")
		return
	}

	s.render(w, r, "enrollment_preview.html", page{
		Title: "確認 " + preview.DisplayName + " 的 enroll 票", Nav: "machines-enrollment",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		EnrollPreview: &enrollPreviewResult{
			Name: preview.DisplayName, Reason: strings.TrimSpace(r.FormValue("reason")),
			PreviewDigest: preview.PreviewDigest, IdempotencyKey: key,
			ExpiresAt:                  preview.ExpiresAtIfCreatedNow.Local().Format("2006-01-02 15:04"),
			TTL:                        (time.Duration(preview.TTLSeconds) * time.Second).String(),
			CreatesExpectedMachine:     preview.CreatesExpectedMachine,
			InitialState:               preview.InitialState,
			RevocationKeepsRegistryRow: preview.RevocationKeepsRegistryRow,
			SecretDelivery:             preview.SecretDelivery,
		},
	})
}

func validateEnrollPreviewRenderResult(result operator.EnrollTokenPreviewResult) error {
	if result.DisplayName == "" || result.TTLSeconds != enrollTTLSeconds || result.PreviewDigest == "" ||
		result.PreviewedAt.IsZero() || result.ExpiresAtIfCreatedNow.IsZero() {
		return errors.New("missing enrollment preview identity, digest, or fixed expiry")
	}
	if !result.ExpiresAtIfCreatedNow.Equal(result.PreviewedAt.Add(enrollTTL)) {
		return errors.New("enrollment preview expiry does not match fixed TTL")
	}
	if !result.CreatesExpectedMachine || result.InitialState != store.OperatorEnrollTokenInitialStateNeverReported ||
		!result.RevocationKeepsRegistryRow || result.SecretDelivery != store.OperatorEnrollTokenSecretDeliveryFirstOnly {
		return errors.New("unexpected enrollment impact policy")
	}
	return nil
}

// doEnrollToken 開一張票，並且把明文**只顯示這一次**。
//
// ⚠⚠ 這條路徑跟 retire 的風險是不同類的：
//
// retire 可以反悔 —— 按錯了就 unretire，狀態回得去。
// 一張畫在畫面上的 token **收不回來**：它在被畫出來的那一刻就已經進了
// 瀏覽器歷史、可能進了截圖、進了任何一個看得到那個畫面的人眼裡。
// `RevokeEnrollToken` 能做的是讓那串字**不能再兌換**，不是讓它消失。
//
// 所以這一頁的三條規則：
//
//  1. **不留、不重顯。** 資料庫裡只有 hash，這一頁重新整理就沒了。
//     沒有「顯示上一次發的 token」這種功能 —— 那等於把一次性變成永久。
//  2. **audit 記「發過一張給誰」，不記 token 本身。**
//     一個把憑證寫進 audit 的系統，是把稽核紀錄變成第二個外洩點。
//  3. **畫面上要講出「開票就進分母」。** 開票的那一刻名冊就多一台，
//     而它會亮紅燈直到報到 —— 那是刻意的，但沒講的話會被當成 bug。
func (s *Server) doEnrollToken(w http.ResponseWriter, r *http.Request) {
	// Production's operator boundary already sets this globally; keep the
	// secret-bearing handler safe when embedded or tested without that wrapper.
	w.Header().Set("Cache-Control", "no-store")
	result, err := s.operator.CreateEnrollToken(operator.EnrollTokenCreateRequest{
		DisplayName:    r.FormValue("name"),
		TTLSeconds:     enrollTTLSeconds,
		PreviewDigest:  r.FormValue("preview_digest"),
		Reason:         r.FormValue("reason"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enroll create 失敗: %v", err)
		}
		s.renderActionStatus(w, r, status, strings.TrimSpace(r.FormValue("name")),
			"沒有開 enroll 票", detail, "/machines/enrollment")
		return
	}
	if err := validateEnrollTokenRenderResult(result); err != nil {
		// Never log the result: a malformed fresh result may contain the one-time
		// secret.  The flags below are enough to diagnose a service-contract bug.
		log.Printf("operator enroll create 回傳不一致：%v（replayed=%t secret_available=%t recovery_required=%t）",
			err, result.Replayed, result.SecretAvailable, result.RecoveryRequired)
		s.renderActionStatus(w, r, http.StatusInternalServerError, result.DisplayName,
			"沒有顯示 enroll 票", "建立結果不符合一次性 secret 契約；請從 audit 確認結果，勿直接重送。", "/machines/enrollment")
		return
	}

	title := result.DisplayName + " 的 enroll 票"
	if result.Replayed {
		title = result.DisplayName + " 的 enroll 票"
	}

	s.render(w, r, "token.html", page{
		Title: title, Nav: "machines-enrollment",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		Token: &tokenResult{
			Name: result.DisplayName, MachineID: result.MachineID,
			Token: result.EnrollmentToken, SecretAvailable: result.SecretAvailable,
			Replayed: result.Replayed, RecoveryRequired: result.RecoveryRequired,
			RecoveryAction: result.RecoveryAction,
			ExpiresAt:      result.ExpiresAt.Local().Format("2006-01-02 15:04"),
			TTL:            (time.Duration(result.TTLSeconds) * time.Second).String(),
			HubBase:        s.hubBase,
		},
	})
}

func validateEnrollTokenRenderResult(result operator.EnrollTokenCreateResult) error {
	if result.MachineID == "" || result.DisplayName == "" || result.TTLSeconds != enrollTTLSeconds ||
		result.PreviewDigest == "" || result.CreatedAt.IsZero() || result.ExpiresAt.IsZero() {
		return errors.New("missing enrollment identity or expiry")
	}
	if !result.ExpiresAt.Equal(result.CreatedAt.Add(enrollTTL)) {
		return errors.New("enrollment expiry does not match fixed TTL")
	}
	if result.Replayed {
		if result.EnrollmentToken != "" || result.SecretAvailable || !result.RecoveryRequired ||
			result.RecoveryAction != store.OperatorEnrollTokenRecoveryRevokeAndReissue {
			return errors.New("replay did not redact secret or prescribe recovery")
		}
		return nil
	}
	if result.EnrollmentToken == "" || !result.SecretAvailable || result.RecoveryRequired || result.RecoveryAction != "" {
		return errors.New("fresh result did not contain exactly one recoverable secret")
	}
	return nil
}

// previewRevokeToken is a read-only review of the exact pending enrollment
// ticket and the fixed impact policy.  It deliberately does not inspect or
// mutate active agent credentials: those are a different lifecycle.
func (s *Server) previewRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	preview, err := s.operator.PreviewEnrollTokenRevocation(
		operator.EnrollTokenRevocationPreviewRequest{MachineID: id})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator pending enrollment ticket revocation preview 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id,
			"沒有建立待兌換 enroll 票的撤銷預覽", detail, "/machines/"+id)
		return
	}
	if err := validateEnrollTokenRevocationPreviewRenderResult(id, preview); err != nil {
		log.Printf("operator pending enrollment ticket revocation preview 回傳不一致 machine=%s: %v", id, err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, id,
			"沒有建立待兌換 enroll 票的撤銷預覽",
			"預覽結果不符合名冊、管理分母與 agent credential 影響契約。", "/machines/"+id)
		return
	}

	key, err := operator.NewIdempotencyKey("web-enroll-token-revoke")
	if err != nil {
		log.Printf("operator pending enrollment ticket revocation preview 產生 idempotency key 失敗 machine=%s: %v", id, err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.DisplayName,
			"沒有建立待兌換 enroll 票的撤銷預覽",
			"無法產生這次確認所需的 request key；請回機器頁重試。", "/machines/"+id)
		return
	}

	// The page carries a one-attempt request key. It is not a bearer secret,
	// but caching it would make a later browser revisit unexpectedly replay an
	// old operator decision.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "revoke_token_preview.html", page{
		Title: "確認撤銷 " + preview.DisplayName + " 的待兌換 enroll 票", Nav: "machines-detail",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		EnrollTokenRevocationPreview: &enrollTokenRevocationPreviewResult{
			MachineID: preview.MachineID, Name: preview.DisplayName,
			Reason:        strings.TrimSpace(r.FormValue("reason")),
			PreviewDigest: preview.PreviewDigest, IdempotencyKey: key,
			TokenCreatedAt: preview.TokenCreatedAt.Local().Format("2006-01-02 15:04:05 MST"),
			TokenExpiresAt: preview.TokenExpiresAt.Local().Format("2006-01-02 15:04:05 MST"),
			PreviewedAt:    preview.PreviewedAt.Local().Format("2006-01-02 15:04:05 MST"),
			TokenExpired:   preview.TokenExpired, RegistryRetained: preview.RegistryRetained,
			DenominatorDelta:              preview.DenominatorDelta,
			ActiveAgentCredentialAffected: preview.ActiveAgentCredentialAffected,
		},
	})
}

func validateEnrollTokenRevocationPreviewRenderResult(machineID string,
	result operator.EnrollTokenRevocationPreviewResult,
) error {
	if result.MachineID == "" || result.MachineID != machineID || result.DisplayName == "" ||
		result.TokenCreatedAt.IsZero() || result.TokenExpiresAt.IsZero() || result.PreviewedAt.IsZero() ||
		len(result.PreviewDigest) != len("sha256:")+64 || !strings.HasPrefix(result.PreviewDigest, "sha256:") {
		return errors.New("missing or mismatched pending enrollment ticket identity")
	}
	if !result.TokenExpiresAt.After(result.TokenCreatedAt) || result.PreviewedAt.Before(result.TokenCreatedAt) ||
		result.TokenExpired != !result.PreviewedAt.Before(result.TokenExpiresAt) {
		return errors.New("pending enrollment ticket timestamps are inconsistent")
	}
	if !result.RegistryRetained || result.DenominatorDelta != 0 || result.ActiveAgentCredentialAffected {
		return errors.New("unexpected pending enrollment ticket revocation policy")
	}
	return nil
}

// doRevokeToken confirms revocation of the exact pending enrollment ticket
// reviewed above.  It does not append a second best-effort audit row: the
// canonical operator service owns the atomic delete, receipt, idempotency and
// original audit evidence.
func (s *Server) doRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := strings.TrimSpace(r.FormValue("reason"))
	w.Header().Set("Cache-Control", "no-store")
	result, err := s.operator.RevokeEnrollToken(operator.EnrollTokenRevocationRequest{
		MachineID: id, PreviewDigest: r.FormValue("preview_digest"),
		Reason:         reason,
		IdempotencyKey: r.FormValue("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			detail = "這是原 request 的回放判決，沒有再次變更 pending enrollment ticket：" + detail
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator pending enrollment ticket revocation 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id,
			"沒有撤銷待兌換的 enroll 票", detail, "/machines/"+id)
		return
	}
	headline := "已撤銷 pending enrollment ticket"
	if result.Replayed {
		headline = "已確認原本的 pending enrollment ticket 撤銷結果（replay）"
	}
	reasonReceipt := "未填"
	if reason != "" {
		reasonReceipt = reason
	}
	detail := fmt.Sprintf(
		"reason=%q；revoked_at=%s；token_was_expired=%t；名冊列保留；管理分母變化 0；active agent credential 未受影響。",
		reasonReceipt, result.RevokedAt.Local().Format("2006-01-02 15:04:05 MST"), result.TokenWasExpired)
	s.render(w, r, "action.html", page{
		Title: headline, Nav: "machines-detail",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		Action: &actionResult{
			Subject: result.DisplayName, Headline: headline, Detail: detail,
			Back: "/machines/" + id, BackLabel: "回那台機器", Changed: true,
		},
	})
}

// tokenResult 是「票只出現這一次」的那一頁。
type tokenResult struct {
	Name, MachineID, Token string
	ExpiresAt, TTL         string
	SecretAvailable        bool
	Replayed               bool
	RecoveryRequired       bool
	RecoveryAction         string
	// HubBase 空字串代表 serve path 沒有提供可信的部署位址。
	HubBase string
}

type enrollPreviewResult struct {
	Name, Reason, PreviewDigest, IdempotencyKey string
	ExpiresAt, TTL                              string
	CreatesExpectedMachine                      bool
	InitialState                                string
	RevocationKeepsRegistryRow                  bool
	SecretDelivery                              string
}

type enrollTokenRevocationPreviewResult struct {
	MachineID, Name, Reason, PreviewDigest, IdempotencyKey string
	TokenCreatedAt, TokenExpiresAt, PreviewedAt            string
	TokenExpired, RegistryRetained                         bool
	DenominatorDelta                                       int64
	ActiveAgentCredentialAffected                          bool
}

// doConnect 記下「有人要連過去」，然後把瀏覽器轉過去。
//
// ⚠⚠ 這裡**不是代理**。轉址之後 Hub 就完全離開那條路徑了 ——
// 它從來沒有拿到過 BAT 的憑證，也不轉發任何一個位元組。
// 這一頁同時把位址印成可複製的文字，所以人隨時可以繞過這個按鈕；
// 那是刻意的，而且 audit 頁面上會講出來：這裡記的是**走這個按鈕的**連線，
// 不是「所有的連線」。一個宣稱自己記下了全部連線的 audit 是在說謊。
func (s *Server) doConnect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	machine, err := s.operator.MachineDetail(id, time.Now().UTC())
	if err != nil {
		s.recordMachineConnectWebAudit(r, operator.MachineConnectAuditRequest{
			Outcome: operator.MachineConnectAuditDetailUnavailable, MachineID: id,
		})
		http.NotFound(w, r)
		return
	}
	connect, err := s.operator.MachineConnect(machine)
	if err != nil {
		s.recordMachineConnectWebAudit(r, operator.MachineConnectAuditRequest{
			Outcome: operator.MachineConnectAuditProjectionUnavailable, MachineID: id,
		})
		http.Error(w, "讀取 BAT 連線座標失敗。", http.StatusInternalServerError)
		return
	}
	reason := r.FormValue("reason")

	if !connect.Available || connect.URL == nil {
		// ⚠ 連不上也要記。「有人試著連 sampleagent2，而它綁在 localhost」
		// 正好是事後最想知道的那種事 —— 它會告訴你哪一台該去改設定了。
		s.recordMachineConnectWebAudit(r, operator.MachineConnectAuditRequest{
			Outcome: operator.MachineConnectAuditAddressUnavailable, MachineID: id,
			Connect: &connect, Reason: reason,
		})
		s.renderActionResult(w, r, connect.DisplayName,
			"這台給不出可以連過去的位址", connect.Why.Text, "/machines/"+id)
		return
	}

	s.recordMachineConnectWebAudit(r, operator.MachineConnectAuditRequest{
		Outcome: operator.MachineConnectAuditRedirected, MachineID: id,
		Connect: &connect, Reason: reason,
	})
	// 303：讓瀏覽器用 GET 去打那個位址，而且重新整理不會再 POST 一次。
	http.Redirect(w, r, connect.URL.Text, http.StatusSeeOther)
}

// Connect audit is observational and best-effort: failure to write the row
// does not change whether the already measured redirect is available. The
// operator service owns the row shape; Web supplies only typed outcome,
// measured projection, reason, and the boundary's verified actor.
func (s *Server) recordMachineConnectWebAudit(r *http.Request, request operator.MachineConnectAuditRequest) {
	request.Actor = operator.ActorFromRequest(r, operator.SourceKindWeb)
	if err := s.operator.RecordMachineConnectAudit(request); err != nil {
		log.Printf("operator machine connect Web audit failed outcome=%s: %v", request.Outcome, err)
	}
}

type lifecyclePreviewView struct {
	MachineID, DisplayName, CurrentState, DesiredState                  string
	Reason, PreviewDigest, IdempotencyKey                               string
	Revision                                                            int64
	InDenominatorBefore, InDenominatorAfter                             bool
	DenominatorDelta                                                    int64
	RegistryRetained, HistoryPreserved, ChannelPreserved                bool
	Channel                                                             string
	ChannelRevision                                                     int64
	AgentCredentialPresent                                              bool
	AgentAuthenticationBefore, AgentAuthenticationAfter                 bool
	PendingEnrollmentTokenCount, PendingEnrollmentTokenExpiredCount     int64
	PendingEnrollmentRedemptionBefore, PendingEnrollmentRedemptionAfter bool
	ActiveJobCount                                                      int64
	Blockers                                                            []store.MachineLifecycleBlocker
}

func (s *Server) previewMachineLifecycle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.renderActionStatus(w, r, http.StatusBadRequest, id,
			"沒有建立生命週期預覽", "生命週期變更必須填理由；預覽沒有鑄造 request key，也沒有寫 audit。", "/machines/"+id)
		return
	}
	revision, parseErr := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	var expected *int64
	if parseErr == nil {
		expected = &revision
	}
	preview, err := s.operator.PreviewMachineLifecycle(operator.MachineLifecyclePreviewRequest{
		MachineID: id, DesiredState: store.MachineLifecycleState(r.FormValue("desired_state")),
		ExpectedRevision: expected,
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator lifecycle preview 失敗 machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id, "沒有建立生命週期預覽", detail, "/machines/"+id)
		return
	}
	key, err := operator.NewIdempotencyKey("web-machine-lifecycle")
	if err != nil {
		log.Printf("operator lifecycle preview 產生 idempotency key 失敗 machine=%s: %v", id, err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.DisplayName,
			"沒有建立生命週期預覽", "無法產生這次確認所需的 request key。請重新預覽。", "/machines/"+id)
		return
	}
	s.render(w, r, "lifecycle_preview.html", page{
		Title: "確認 " + preview.DisplayName + " 的生命週期變更", Nav: "machines-detail",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		LifecyclePreview: &lifecyclePreviewView{
			MachineID: preview.MachineID, DisplayName: preview.DisplayName,
			CurrentState: string(preview.CurrentState), DesiredState: string(preview.DesiredState),
			Reason: reason, PreviewDigest: preview.PreviewDigest,
			IdempotencyKey: key, Revision: preview.LifecycleRevision,
			InDenominatorBefore: preview.InDenominatorBefore, InDenominatorAfter: preview.InDenominatorAfter,
			DenominatorDelta: preview.DenominatorDelta, RegistryRetained: preview.RegistryRetained,
			HistoryPreserved: preview.HistoryPreserved, Channel: preview.Channel,
			ChannelRevision: preview.ChannelRevision, ChannelPreserved: preview.ChannelPreserved,
			AgentCredentialPresent:             preview.AgentCredentialPresent,
			AgentAuthenticationBefore:          preview.AgentAuthenticationBefore,
			AgentAuthenticationAfter:           preview.AgentAuthenticationAfter,
			PendingEnrollmentTokenCount:        preview.PendingEnrollmentTokenCount,
			PendingEnrollmentTokenExpiredCount: preview.PendingEnrollmentTokenExpiredCount,
			PendingEnrollmentRedemptionBefore:  preview.PendingEnrollmentRedemptionBefore,
			PendingEnrollmentRedemptionAfter:   preview.PendingEnrollmentRedemptionAfter,
			ActiveJobCount:                     preview.ActiveJobCount, Blockers: preview.Blockers,
		},
	})
}

// The legacy route names remain stable bookmarks/forms, but both now enter
// the same canonical lifecycle service and cannot bypass preview, revision,
// typed confirmation, idempotency, or atomic audit.
func (s *Server) doRetire(w http.ResponseWriter, r *http.Request) {
	s.applyMachineLifecycle(w, r, store.MachineLifecycleRetired)
}

func (s *Server) doUnretire(w http.ResponseWriter, r *http.Request) {
	s.applyMachineLifecycle(w, r, store.MachineLifecycleActive)
}

func (s *Server) applyMachineLifecycle(w http.ResponseWriter, r *http.Request, desired store.MachineLifecycleState) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	revision, parseErr := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	var expected *int64
	if parseErr == nil {
		expected = &revision
	}
	result, err := s.operator.ChangeMachineLifecycle(operator.MachineLifecycleRequest{
		MachineID: id, DesiredState: desired, ExpectedRevision: expected,
		ConfirmDisplayName: r.FormValue("confirm"), PreviewDigest: r.FormValue("preview_digest"),
		Reason: r.FormValue("reason"), IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			detail = "這是原 request 的回放判決，未重新評估目前狀態：" + detail
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator lifecycle apply 失敗 machine=%s desired=%s: %v", id, desired, err)
		}
		s.renderActionStatus(w, r, status, id, "沒有變更機器生命週期", detail, "/machines/"+id)
		return
	}
	if result.MachineID != id || result.State != desired || result.DisplayName == "" {
		log.Printf("operator lifecycle apply 回傳不一致 machine=%s desired=%s", id, desired)
		s.renderActionStatus(w, r, http.StatusInternalServerError, id,
			"沒有確認機器生命週期結果", "控制面回傳的 canonical receipt 不一致。", "/machines/"+id)
		return
	}
	http.Redirect(w, r, "/machines/"+id+"?section=actions#actions", http.StatusSeeOther)
}

// renderActionResult 是「按了但沒做成」的那一頁。
//
// ⚠ 不用 http.Error：那會給一個沒有導覽、沒有回頭路的白畫面，
// 而人在那個畫面上唯一會做的事是按上一頁然後再按一次同一個按鈕。
func (s *Server) renderActionResult(w http.ResponseWriter, r *http.Request,
	subject, headline, detail, back string) {
	s.renderActionStatus(w, r, http.StatusOK, subject, headline, detail, back)
}

func (s *Server) renderActionStatus(w http.ResponseWriter, r *http.Request, status int,
	subject, headline, detail, back string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	nav := ""
	if back == "/machines/enrollment" || strings.HasPrefix(back, "/machines/enrollment?") {
		nav = "machines-enrollment"
	} else if back == "/machines/diagnostics" {
		nav = "machines-diagnostics"
	} else if strings.HasPrefix(back, "/machines/") {
		nav = "machines-detail"
	} else if strings.HasPrefix(back, "/deployments/") {
		nav = "deployments-detail"
	} else if strings.HasPrefix(back, "/jobs/") {
		nav = "jobs-detail"
	} else if back == "/tenant/maintenance" {
		nav = "tenant-maintenance"
	}
	s.render(w, r, "action.html", page{
		Title: headline, Nav: nav,
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		Action: &actionResult{
			Subject: subject, Headline: headline, Detail: detail, Back: back,
			BackLabel: actionBackLabel(back),
		},
	})
}

type actionResult struct {
	Subject, Headline, Detail, Back, BackLabel string
	Changed                                    bool
}

func actionBackLabel(back string) string {
	switch {
	case back == "/machines/enrollment" || strings.HasPrefix(back, "/machines/enrollment?"):
		return "回機器註冊"
	case back == "/machines/diagnostics":
		return "回診斷"
	case strings.HasPrefix(back, "/machines/"):
		return "回那台機器"
	case strings.HasPrefix(back, "/deployments/"):
		return "回該部署"
	case back == "/" || strings.HasPrefix(back, "/?"):
		return "回總覽"
	case back == "/tenant/maintenance":
		return "回維護"
	default:
		return "回上一個控制面頁面"
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
