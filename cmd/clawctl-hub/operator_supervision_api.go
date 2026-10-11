package main

import (
	"net/http"
	"net/url"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func parseSupervisionQuery(r *http.Request, jobs bool) (operator.SupervisionFilter, error) {
	f := operator.SupervisionFilter{}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return f, operator.ErrInvalidSupervision
	}
	for key := range values {
		if key != "window" && (!jobs || (key != "kind" && key != "machine_id" && key != "per_machine")) {
			return f, operator.ErrInvalidSupervision
		}
		v, e := oneOperatorAuditQueryValue(values, key, 256)
		if e != nil {
			return f, operator.ErrInvalidSupervision
		}
		switch key {
		case "window":
			f.Window = v
		case "kind":
			f.Kind = v
		case "machine_id":
			f.MachineID = v
		case "per_machine":
			if v != "true" && v != "false" {
				return f, operator.ErrInvalidSupervision
			}
			f.PerMachine = v == "true"
		}
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return f, operator.ErrInvalidSupervision
	}
	return f, operator.ValidateSupervisionFilter(f)
}

func (h *hub) handleOperatorJobsSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	f, err := parseSupervisionQuery(r, true)
	if err != nil {
		writeErr(w, 400, "BAD_REQUEST", "invalid supervision filter")
		return
	}
	result, err := operator.New(h.store).JobsSummary(r.Context(), f, time.Now().UTC())
	if err != nil {
		writeErr(w, 500, "INTERNAL", "supervision read failed")
		return
	}
	writeJSON(w, 200, result)
}
func (h *hub) handleOperatorApprovalsSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	f, err := parseSupervisionQuery(r, false)
	if err != nil {
		writeErr(w, 400, "BAD_REQUEST", "invalid supervision filter")
		return
	}
	result, err := operator.New(h.store).ApprovalsSummary(r.Context(), f.Window, time.Now().UTC())
	if err != nil {
		writeErr(w, 500, "INTERNAL", "supervision read failed")
		return
	}
	writeJSON(w, 200, result)
}
func (h *hub) handleOperatorHubStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeErr(w, 400, "BAD_REQUEST", "hub status accepts no query")
		return
	}
	result, err := operator.New(h.store).HubStatus(r.Context(), version, h.startedAt, time.Now().UTC())
	if err != nil {
		writeErr(w, 500, "INTERNAL", "supervision read failed")
		return
	}
	writeJSON(w, 200, result)
}
