package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

const terminalOpenRefused = "沒有開啟終端"

// openTerminal records one terminal session and sends the browser to its page.
// session_id and idempotency_key come from the machine page. Minting either
// here would make one double submit into two sessions.
func (s *Server) openTerminal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	machineID := r.PathValue("id")
	if s.terminalLinks == nil || !s.terminalLinks.HasLink(machineID) {
		s.refuseTerminalOpen(w, r, machineID, http.StatusConflict,
			"這台的終端連線目前沒有接上 Hub。請確認機器上的 agent 與 bat-server 都在執行，再回到機器頁開啟。")
		return
	}
	principal, _ := operatorauth.PrincipalFromContext(r.Context())
	result, err := s.operator.OpenAgentSession(operator.AgentSessionOpenRequest{
		SessionID: r.FormValue("session_id"), MachineID: machineID,
		IdempotencyKey: r.FormValue("idempotency_key"), Principal: principal,
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, detail := terminalOpenRefusal(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator terminal open failed machine=%s: %v", machineID, err)
		}
		s.refuseTerminalOpen(w, r, machineID, status, detail)
		return
	}
	http.Redirect(w, r, "/machines/"+result.MachineID+"/terminals/"+result.SessionID, http.StatusSeeOther)
}

func (s *Server) refuseTerminalOpen(w http.ResponseWriter, r *http.Request, machineID string, status int, detail string) {
	s.renderActionStatus(w, r, status, machineID, terminalOpenRefused, detail, "/machines/"+machineID)
}

func terminalOpenRefusal(err error) (int, string) {
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) {
		return http.StatusInternalServerError, "目前無法開啟終端。請回到機器頁再試一次。"
	}
	switch rejection.Code {
	case store.OperatorCodeAgentSessionMachineNotFound:
		return http.StatusNotFound, machineNotOnRoster
	case store.OperatorCodeAgentSessionMachineRetired:
		return http.StatusConflict, "這台機器已退役，不能開啟終端。"
	case store.OperatorCodeAgentSessionMachineUnassigned:
		return http.StatusForbidden, "這台機器沒有指派使用者，不能開啟終端。"
	case store.OperatorCodeAgentSessionAssignedUserMismatch:
		return http.StatusForbidden, "只有這台機器的指派使用者可以開啟它的終端。"
	case store.OperatorCodeAgentSessionOperatorUserRequired:
		return http.StatusForbidden, "這個連線來源沒有 tailnet 使用者身分，不能開啟終端。"
	case store.OperatorCodeAgentSessionLimitReached:
		return http.StatusConflict, fmt.Sprintf(
			"這台機器已有 %d 個開啟中的終端，已達上限。請先關閉其中一個終端的分頁，再回到機器頁開啟。",
			store.MaxOpenAgentSessionsPerMachine)
	case store.OperatorCodeAgentSessionExists:
		return http.StatusConflict, "這次開啟已經用過。請重新整理機器頁再開啟。"
	case store.OperatorCodeAgentSessionClosed:
		return http.StatusConflict, "這次開啟已經用過，它開的終端已結束。請重新整理機器頁再開啟。"
	case store.OperatorCodeIdempotencyKeyRequired, store.OperatorCodeAgentSessionInvalid, store.OperatorCodeIdempotencyConflict:
		return http.StatusBadRequest, "開啟終端的請求不完整。請重新整理機器頁再開啟。"
	default:
		return http.StatusInternalServerError, "目前無法開啟終端。請回到機器頁再試一次。"
	}
}
