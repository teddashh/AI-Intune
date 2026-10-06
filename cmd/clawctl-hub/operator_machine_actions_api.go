package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

// handleGetOperatorMachineActions answers "what can I do to this machine right
// now". It is a view read: the catalogue reports state, never changes it.
//
// 目錄只列出這個 principal 真的握有 capability 的動作。view-only 的操作員拿到的
// 不是一串做不了的按鈕，而是一份對他而言為真的清單。
func (h *hub) handleGetOperatorMachineActions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine actions 不接受 query parameters")
		return
	}
	machineID := r.PathValue("id")
	service := operator.New(h.store)
	detail, err := service.MachineDetail(machineID, time.Now().UTC())
	if err != nil {
		writeOperatorMachineActionsError(w, err, machineID)
		return
	}
	connect, err := service.MachineConnect(detail)
	if err != nil {
		writeOperatorMachineActionsError(w, err, machineID)
		return
	}
	lifecycle, err := service.MachineLifecycle(machineID)
	if err != nil {
		writeOperatorMachineActionsError(w, err, machineID)
		return
	}
	assigned, err := service.MachineAssignedUser(machineID)
	if err != nil {
		writeOperatorMachineActionsError(w, err, machineID)
		return
	}
	principal, _ := operatorauth.PrincipalFromContext(r.Context())
	catalogue, err := service.MachineActions(operator.MachineActionsRequest{
		Detail: detail, Connect: connect, Lifecycle: lifecycle,
		Granted: operator.MachineActionGrant{
			Operate: principal.Has(operatorauth.Operate),
			Admin:   principal.Has(operatorauth.Admin),
		},
		OperatorTailnetUserID: principal.TailnetUserID,
		AssignedUserID:        assigned.UserID,
		TerminalLinked:        h.agentLinks.HasLink(machineID),
	})
	if err != nil {
		writeOperatorMachineActionsError(w, err, machineID)
		return
	}
	writeJSON(w, http.StatusOK, catalogue)
}

func writeOperatorMachineActionsError(w http.ResponseWriter, err error, machineID string) {
	var rejection *store.OperatorRequestError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
	case errors.As(err, &rejection) && rejection.Code == store.OperatorCodeMachineNotFound:
		writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
	default:
		log.Printf("讀取 operator machine actions 失敗 machine=%q: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取裝置動作目錄失敗")
	}
}
