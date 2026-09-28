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

type diagnosticPageView struct {
	Rows []diagnosticPageRow
}

type diagnosticPageRow struct {
	MachineID   string
	DisplayName string
	State       string
}

type diagnosticPreviewView struct {
	MachineID       string
	DisplayName     string
	Reason          string
	PreviewDigest   string
	IdempotencyKey  string
	Timeout         int
	CurrentRevision int64
	PlannedRevision int64
	ActiveJobCount  int64
	JobsEnabled     string
	Blockers        []string
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	result, err := s.operator.ListMachines(time.Now().UTC())
	if err != nil {
		s.fail(w, "讀取診斷目標失敗", err)
		return
	}
	overview, ok := result.StoreOverview()
	if !ok {
		s.fail(w, "讀取診斷目標失敗", errors.New("operator machine overview unavailable"))
		return
	}
	view := &diagnosticPageView{Rows: make([]diagnosticPageRow, 0, len(overview.Machines))}
	for _, machine := range overview.Machines {
		view.Rows = append(view.Rows, diagnosticPageRow{
			MachineID: machine.MachineID, DisplayName: machine.DisplayName, State: string(machine.State),
		})
	}
	s.render(w, r, "diagnostics.html", page{
		Title: "診斷", Nav: "machines-diagnostics",
		Now: time.Now().Local().Format("2006-01-02 15:04"), Diagnostics: view,
	})
}

func (s *Server) previewDiagnosticNoop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.renderActionStatus(w, r, http.StatusBadRequest, id,
			"診斷預覽失敗", "請填寫理由。", "/machines/diagnostics")
		return
	}
	timeout, err := strconv.Atoi(r.FormValue("timeout"))
	if err != nil {
		s.renderActionStatus(w, r, http.StatusBadRequest, id,
			"診斷預覽失敗", "逾時秒數格式錯誤。", "/machines/diagnostics")
		return
	}
	preview, err := s.operator.PreviewDiagnosticNoop(operator.DiagnosticNoopPreviewRequest{
		MachineID: id, ExecutionTimeoutSeconds: timeout,
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator diagnostic preview failed machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id, "診斷預覽失敗", detail, "/machines/diagnostics")
		return
	}
	key, err := operator.NewIdempotencyKey("web-diagnostic-noop")
	if err != nil {
		log.Printf("operator diagnostic preview key failed machine=%s: %v", id, err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, preview.DisplayName,
			"診斷預覽失敗", "無法建立確認。請重試。", "/machines/diagnostics")
		return
	}
	view := &diagnosticPreviewView{
		MachineID: preview.MachineID, DisplayName: preview.DisplayName,
		Reason: reason, PreviewDigest: preview.PreviewDigest, IdempotencyKey: key,
		Timeout:         preview.ExecutionTimeoutSeconds,
		CurrentRevision: int64(preview.CurrentResourceRevision), PlannedRevision: int64(preview.PlannedRevision),
		ActiveJobCount: preview.ActiveJobCount,
		JobsEnabled:    diagnosticExecutionLabel(preview.JobsEnabled),
	}
	for _, blocker := range preview.Blockers {
		view.Blockers = append(view.Blockers, diagnosticBlockerLabel(blocker, preview.ActiveJobCount))
	}
	s.render(w, r, "diagnostic_noop_preview.html", page{
		Title: "確認 " + preview.DisplayName + " 的診斷", Nav: "machines-diagnostics",
		Now: time.Now().Local().Format("2006-01-02 15:04"), DiagnosticPreview: view,
	})
}

func diagnosticBlockerLabel(blocker store.OperatorDiagnosticNoopBlocker, activeJobs int64) string {
	switch blocker {
	case store.OperatorDiagnosticNoopBlockerRetired:
		return "機器已退役"
	case store.OperatorDiagnosticNoopBlockerNeverReported:
		return "尚無 agent 回報"
	case store.OperatorDiagnosticNoopBlockerNonterminalJob:
		return fmt.Sprintf("%d 張工作單執行中", activeJobs)
	case store.OperatorDiagnosticNoopBlockerExecutionUnknown:
		return "Agent 尚未回報工作單執行狀態；等待下一次 check-in 後重新預覽"
	case store.OperatorDiagnosticNoopBlockerExecutionDisabled:
		return "Agent 工作單執行未啟用；將 jobs_enabled 設為 true 並重新啟動 clawctl-agent"
	default:
		return string(blocker)
	}
}

func diagnosticExecutionLabel(enabled *bool) string {
	if enabled == nil {
		return "等待回報"
	}
	if *enabled {
		return "已啟用"
	}
	return "未啟用"
}

func (s *Server) applyDiagnosticNoop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	timeout, err := strconv.Atoi(r.FormValue("timeout"))
	if err != nil {
		s.renderActionStatus(w, r, http.StatusBadRequest, id,
			"診斷建立失敗", "逾時秒數格式錯誤。", "/machines/diagnostics")
		return
	}
	result, err := s.operator.CreateDiagnosticNoop(operator.DiagnosticNoopRequest{
		MachineID: id, ExecutionTimeoutSeconds: timeout,
		ConfirmDisplayName: r.FormValue("confirm"), PreviewDigest: r.FormValue("preview_digest"),
		Reason: r.FormValue("reason"), IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator diagnostic apply failed machine=%s: %v", id, err)
		}
		s.renderActionStatus(w, r, status, id, "診斷建立失敗", detail, "/machines/diagnostics")
		return
	}
	if result.MachineID != id || result.DisplayName == "" || result.JobID == "" || result.DesiredID == "" {
		log.Printf("operator diagnostic apply inconsistent machine=%s", id)
		s.renderActionStatus(w, r, http.StatusInternalServerError, id,
			"診斷建立失敗", "工作單結果不完整。", "/machines/diagnostics")
		return
	}
	http.Redirect(w, r, "/jobs/"+result.JobID, http.StatusSeeOther)
}
