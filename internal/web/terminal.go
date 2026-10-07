package web

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/sessionid"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web/terminalassets"
)

const (
	machineNotOnRoster     = "名冊上沒有這台機器。"
	terminalSessionMissing = "找不到這個終端工作階段。請回到機器頁重新開啟。"
)

type terminalPage struct {
	CSS   template.CSS
	XTerm template.JS
	Fit   template.JS
	Page  template.JS
	// IdleTimeout and MaxLifetime are zh-Hant durations ("30 分鐘",
	// "1 小時 30 分鐘"); IdleTimeoutMS feeds the page's cosmetic idle warning.
	IdleTimeout   string
	MaxLifetime   string
	IdleTimeoutMS int64
	Deadline      string
}

// terminalLimitText renders a whole-second duration as zh-Hant without
// rounding, so "90m" reads 1 小時 30 分鐘 rather than 1 小時.
func terminalLimitText(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int64(d / time.Hour)
	m := int64(d % time.Hour / time.Minute)
	sec := int64(d % time.Minute / time.Second)
	var parts []string
	if h > 0 {
		parts = append(parts, strconv.FormatInt(h, 10)+" 小時")
	}
	if m > 0 {
		parts = append(parts, strconv.FormatInt(m, 10)+" 分鐘")
	}
	if sec > 0 || len(parts) == 0 {
		parts = append(parts, strconv.FormatInt(sec, 10)+" 秒")
	}
	return strings.Join(parts, " ")
}

func terminalDocumentAssets() terminalPage {
	return terminalPage{
		CSS:   template.CSS(string(terminalassets.XTermCSS)),
		XTerm: template.JS(string(terminalassets.XTermJS)),
		Fit:   template.JS(string(terminalassets.FitJS)),
		Page:  template.JS(string(terminalassets.PageJS)),
	}
}

// terminalDocument serves the one operator page that runs JavaScript.
//
// The browser speaks aiintune.operator-terminal.v1 as text JSON. No frame in
// either direction carries a session identifier; the socket path is the
// binding. Output bytes are the operator's shell and can contain that
// machine's BAT token when the operator prints the file. Those bytes must
// not be copied into a log, an audit row, persistence, or an error string.
// The page script does not log a frame, and the socket handler that later
// accepts these frames has to keep the same rule.
func (s *Server) terminalDocument(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	machine, err := s.store.GetMachine(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, machineNotOnRoster, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "讀取終端失敗", err)
		return
	}
	// Reject a segment the session grammar does not accept before it reaches
	// the database. Every session miss uses the same message: describing the
	// grammar would tell the caller which check failed.
	sessionID := r.PathValue("session")
	if !sessionid.Valid(sessionID) {
		http.Error(w, terminalSessionMissing, http.StatusNotFound)
		return
	}
	principal, ok := operatorauth.PrincipalFromContext(r.Context())
	if !ok || principal.TailnetUserID == "" {
		http.Error(w, terminalSessionMissing, http.StatusNotFound)
		return
	}
	// The ledger column is the stable tailnet user id, not the login and not
	// the audit subject prefix. Login equality is a different person.
	session, err := s.store.AgentSessionForOperator(machine.MachineID, sessionID, principal.TailnetUserID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, terminalSessionMissing, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "讀取終端失敗", err)
		return
	}
	if session.MachineID != machine.MachineID || session.ClosedAt != nil ||
		session.OperatorTailnetUserID != principal.TailnetUserID {
		http.Error(w, terminalSessionMissing, http.StatusNotFound)
		return
	}
	deadline := session.OpenedAt.Add(s.terminalMaxLifetime).Local().Format("2006-01-02 15:04")
	assets := terminalDocumentAssets()
	assets.IdleTimeout = terminalLimitText(s.terminalIdleTimeout)
	assets.MaxLifetime = terminalLimitText(s.terminalMaxLifetime)
	assets.IdleTimeoutMS = s.terminalIdleTimeout.Milliseconds()
	assets.Deadline = deadline

	s.render(w, r, "terminal.html", page{
		Title: machine.DisplayName + " 的終端",
		Now:   time.Now().Local().Format("2006-01-02 15:04"),
		Machine: machinePageMachine{
			MachineID: machine.MachineID, DisplayName: machine.DisplayName,
		},
		Terminal: assets,
	})
}
