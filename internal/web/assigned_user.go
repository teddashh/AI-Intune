package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

type assignedUserPreview struct {
	MachineID, DisplayName, UserID, CurrentLabel, TargetLabel, Key string
	Revision                                                       int64
}

func assignedUserWebLabel(id, login string) string {
	if login != "" {
		return safeWebDerivedText(login, nil)
	}
	if id != "" {
		return safeWebDerivedText(id, nil)
	}
	return "未指派"
}

func assignedUserDirectory(status tailnet.Status) tailnet.UserDirectory {
	directory := tailnet.Users(status)
	users := make([]tailnet.User, 0, len(directory.Users))
	for _, user := range directory.Users {
		if strings.TrimSpace(user.Login) == "" {
			continue
		}
		user.Login = safeWebDerivedText(user.Login, nil)
		users = append(users, user)
	}
	directory.Users = users
	return directory
}

func (s *Server) previewMachineAssignedUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.store.GetMachine(id)
	if err != nil {
		s.renderActionStatus(w, r, http.StatusNotFound, id, "找不到機器", "機器不存在", "/machines")
		return
	}
	revision, err := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	if err != nil || revision != m.AssignedUserRevision {
		s.renderActionStatus(w, r, http.StatusPreconditionFailed, id, "指派版本已變更", "請重新讀取機器後再指派", "/machines/"+id)
		return
	}
	if m.RetiredAt != nil {
		s.renderActionStatus(w, r, http.StatusConflict, id, "已退役", "恢復管理後可指派使用者", "/machines/"+id)
		return
	}
	userID := r.FormValue("user_id")
	target := "未指派"
	if userID != store.AssignedUserNone {
		directory := assignedUserDirectory(s.tailnet.Get(r.Context()))
		if !directory.Available {
			s.renderActionStatus(w, r, http.StatusServiceUnavailable, id, "來源不可用", "使用者名冊：來源不可用", "/machines/"+id)
			return
		}
		found := false
		for _, user := range directory.Users {
			if user.UserID == userID {
				target = user.Login
				found = true
				break
			}
		}
		if !found {
			s.renderActionStatus(w, r, http.StatusBadRequest, id, "使用者不在名冊", "請重新選擇使用者", "/machines/"+id)
			return
		}
	}
	key, err := operator.NewIdempotencyKey("web-machine-assigned-user")
	if err != nil {
		s.fail(w, "產生請求金鑰失敗", err)
		return
	}
	s.render(w, r, "machine_assigned_user_review.html", page{Title: "確認指派使用者", Nav: "machines-detail", AssignedUserPreview: &assignedUserPreview{
		MachineID: id, DisplayName: m.DisplayName, UserID: userID, Revision: revision, CurrentLabel: assignedUserWebLabel(m.AssignedUserID, m.AssignedUserLogin), TargetLabel: target, Key: key,
	}})
}

func (s *Server) doMachineAssignedUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	revision, err := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	var expected *int64
	if err == nil {
		expected = &revision
	}
	_, err = s.operator.ChangeMachineAssignedUser(r.Context(), operator.MachineAssignedUserRequest{
		MachineID: id, UserID: r.FormValue("user_id"), ExpectedRevision: expected,
		ConfirmDisplayName: r.FormValue("confirm_display_name"), IdempotencyKey: r.FormValue("idempotency_key"), Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if code == store.OperatorCodeTailnetSourceUnavailable {
			detail = "使用者名冊：來源不可用"
		}
		if code == store.OperatorCodeAssignedUserNotInRoster {
			detail = "使用者不在名冊；請重新選擇使用者"
		}
		s.renderActionStatus(w, r, status, id, "指派未完成", detail, fmt.Sprintf("/machines/%s", id))
		return
	}
	http.Redirect(w, r, "/machines/"+id, http.StatusSeeOther)
}
