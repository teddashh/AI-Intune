package web

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// verifierAssignmentPreviewView adds the operator's own words to the Hub's
// preview. Reason travels through the confirmation page so the audit records
// what the operator typed, not what the form defaulted to.
type verifierAssignmentPreviewView struct {
	store.OperatorVerificationAssignmentPreviewResult
	Reason string
}

func (s *Server) previewVerifierAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	jobID := r.PathValue("id")
	back := "/jobs/" + jobID
	verifierID := strings.TrimSpace(r.FormValue("verifier_id"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	if verifierID == "" {
		s.renderActionStatus(w, r, http.StatusBadRequest, jobID,
			"沒有建立派工預覽", "請選一個 verifier。", back)
		return
	}
	if reason == "" || len(reason) > 500 {
		s.renderActionStatus(w, r, http.StatusBadRequest, jobID,
			"沒有建立派工預覽", "理由不可為空，最多 500 bytes。", back)
		return
	}
	preview, err := s.operator.PreviewVerificationAssignment(operator.VerificationAssignmentPreviewRequest{
		VerifierID: verifierID, JobID: jobID,
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verification assignment preview failed: %v", err)
		}
		s.renderActionStatus(w, r, status, jobID, "沒有建立派工預覽", detail, back)
		return
	}
	key, err := operator.NewIdempotencyKey("web-verifier-assign")
	if err != nil {
		s.renderActionStatus(w, r, http.StatusInternalServerError, jobID,
			"沒有建立派工預覽", "無法產生這次確認所需的 request key。", back)
		return
	}
	s.render(w, r, "verifier_assignment_review.html", page{
		Title: "確認派工", Nav: "jobs-detail",
		Now:                       time.Now().Local().Format("2006-01-02 15:04"),
		VerifierAssignmentPreview: &verifierAssignmentPreviewView{preview, reason},
		IdempotencyKey:            key,
	})
}

func (s *Server) applyVerifierAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	jobID := r.PathValue("id")
	back := "/jobs/" + jobID
	verifierID := strings.TrimSpace(r.FormValue("verifier_id"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	confirm := strings.TrimSpace(r.FormValue("confirm_verifier_name"))
	digest := strings.TrimSpace(r.FormValue("preview_digest"))
	key := strings.TrimSpace(r.FormValue("idempotency_key"))
	if verifierID == "" || reason == "" || digest == "" || key == "" {
		s.renderActionStatus(w, r, http.StatusBadRequest, jobID,
			"沒有指派 verifier", "確認表單缺少必要欄位，請重新預覽。", back)
		return
	}
	// The typed name travels down to the writer, which compares it with the
	// registry inside the same transaction. Checking it here instead would be a
	// second, weaker copy of the rule that can drift from the one that decides.
	result, err := s.operator.AssignVerification(operator.VerificationAssignmentRequest{
		VerifierID: verifierID, JobID: jobID, ConfirmVerifierName: confirm,
		PreviewDigest: digest, Reason: reason, IdempotencyKey: key,
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verification assignment failed: %v", err)
		}
		s.renderActionStatus(w, r, status, jobID, "沒有指派 verifier", detail, back)
		return
	}
	if result.JobID != jobID || result.VerifierID != verifierID || result.AssignmentID == "" {
		log.Printf("operator verification assignment receipt inconsistent job=%s", jobID)
		s.renderActionStatus(w, r, http.StatusInternalServerError, jobID,
			"沒有確認派工結果", "控制面回傳的 canonical receipt 不一致。", back)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
