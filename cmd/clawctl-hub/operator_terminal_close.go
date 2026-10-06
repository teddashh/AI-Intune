package main

import "github.com/teddashh/AI-Intune/internal/agentlink"

// installOperatorTerminalSessionCloser wires the process-lifetime operator
// service to the registry the Hub and the web server already share.
// Direct-database commands never call it.
func installOperatorTerminalSessionCloser(h *hub) {
	if h == nil || h.operatorService == nil || h.agentLinks == nil {
		return
	}
	links := h.agentLinks
	h.operatorService.SetTerminalSessionCloser(func(sessionIDs []string) {
		links.CloseSessions(sessionIDs, agentlink.ReasonSessionRevoked)
	})
}
