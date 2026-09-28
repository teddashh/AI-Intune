package web

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// 註冊上限的畫面：這個 Hub 還收不收得下一台。
//
// 它擺在裝置註冊那一頁的最上面，因為它擋的就是那一頁上的那個按鈕。一個藏在設定
// 深處的上限，會讓「為什麼我開不了票」變成一個要問人的問題。
const enrollmentLimitBack = "/machines/enrollment"

func (s *Server) previewEnrollmentLimit(w http.ResponseWriter, r *http.Request) {
	reason := r.FormValue("reason")
	if reason == "" || strings.TrimSpace(reason) != reason || len(reason) > 500 {
		s.renderActionStatus(w, r, http.StatusBadRequest, "註冊上限",
			"未建立上限預覽", "理由不可省略、前後不可有空白，且最多 500 字。", enrollmentLimitBack)
		return
	}
	set, maxMachines, ok := parseEnrollmentLimitForm(r)
	if !ok {
		s.renderActionStatus(w, r, http.StatusBadRequest, "註冊上限",
			"未建立上限預覽", fmt.Sprintf("上限必須是 0 到 %d 之間的整數。", operator.MaxEnrollmentLimitMachines),
			enrollmentLimitBack)
		return
	}
	preview, err := s.operator.PreviewEnrollmentLimit(operator.EnrollmentLimitPreviewRequest{
		Set: set, MaxMachines: maxMachines,
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment limit preview failed: %v", err)
		}
		s.renderActionStatus(w, r, status, "註冊上限", "未建立上限預覽", detail, enrollmentLimitBack)
		return
	}
	key, err := operator.NewIdempotencyKey("web-enrollment-limit")
	if err != nil {
		s.renderActionStatus(w, r, http.StatusInternalServerError, "註冊上限",
			"未建立上限預覽", "無法建立 request key。", enrollmentLimitBack)
		return
	}
	s.render(w, r, "enrollment_limit_review.html", page{
		Title: "確認註冊上限", Nav: "machines-enrollment",
		Now:                   time.Now().Local().Format("2006-01-02 15:04"),
		EnrollmentLimitReview: &preview, EnrollmentLimitReason: reason, IdempotencyKey: key,
	})
}

func (s *Server) applyEnrollmentLimit(w http.ResponseWriter, r *http.Request) {
	set, maxMachines, ok := parseEnrollmentLimitForm(r)
	if !ok {
		s.recordEnrollmentLimitWebTransportRejection(r, operator.EnrollmentLimitTransportRejectionFormInvalid)
		s.renderActionStatus(w, r, http.StatusBadRequest, "註冊上限", "未更改上限",
			"上限不合法；請重新預覽。", enrollmentLimitBack)
		return
	}
	revision, err := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	if err != nil || revision < 0 {
		s.recordEnrollmentLimitWebTransportRejection(r, operator.EnrollmentLimitTransportRejectionRevisionInvalid)
		s.renderActionStatus(w, r, http.StatusBadRequest, "註冊上限", "未更改上限",
			"預覽座標不合法；請重新預覽。", enrollmentLimitBack)
		return
	}
	result, err := s.operator.SetEnrollmentLimit(operator.EnrollmentLimitApplyRequest{
		EnrollmentLimitPreviewRequest: operator.EnrollmentLimitPreviewRequest{
			Set: set, MaxMachines: maxMachines,
		},
		Reason: r.FormValue("reason"), ExpectedRevision: revision,
		PreviewDigest: r.FormValue("preview_digest"), IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, _, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment limit apply failed: %v", err)
		}
		s.renderActionStatus(w, r, status, "註冊上限", "未更改上限", detail, enrollmentLimitBack)
		return
	}
	detail := operator.EnrollmentLimitHeadline(result.State)
	if result.Replayed {
		detail = "已回放原判決；上限沒有再改一次。" + detail
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, r, "action.html", page{
		Title: "註冊上限已更新", Nav: "machines-enrollment",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		Action: &actionResult{Subject: "註冊上限", Headline: "註冊上限已更新", Detail: detail,
			Back: enrollmentLimitBack, BackLabel: actionBackLabel(enrollmentLimitBack), Changed: true},
	})
}

// parseEnrollmentLimitForm reads the two things the form can say: 取消上限,
// or a number.
//
// ⚠ 空白的台數不可以當成 0。「不設上限」跟「上限 0」是相反的兩個意思，一個把空
// 欄位讀成 0 的表單會在有人只想清掉輸入框的時候把整個機隊鎖死。
func parseEnrollmentLimitForm(r *http.Request) (bool, int, bool) {
	if r.FormValue("clear") != "" {
		return false, 0, true
	}
	// ⚠ 不 TrimSpace。一個會自己修掉輸入的欄位，會讓 " 4" 跟 "4" 變成兩個看起來
	// 一樣、canonical request digest 卻不同的請求。
	raw := r.FormValue("max_machines")
	if raw == "" {
		return false, 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(value) != raw || value < 0 || value > operator.MaxEnrollmentLimitMachines {
		return false, 0, false
	}
	return true, value, true
}

func (s *Server) recordEnrollmentLimitWebTransportRejection(r *http.Request, code operator.EnrollmentLimitTransportRejectionCode) {
	err := s.operator.RecordEnrollmentLimitTransportRejection(operator.EnrollmentLimitTransportRejectionRequest{
		Code: code, IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		log.Printf("operator enrollment limit Web transport audit failed code=%s: %v", code, err)
	}
}
