package main

import (
	"bytes"
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"github.com/teddashh/AI-Intune/internal/store"
	"io"
	"net/http"
)

func (h *hub) handleScriptCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeErr(w, 400, "BAD_REQUEST", "query parameters are unsupported")
		return
	}
	writeJSON(w, 200, scriptcatalog.CatalogResponse{Items: scriptcatalog.List()})
}
func (h *hub) handleScriptPreview(w http.ResponseWriter, r *http.Request) {
	h.handleScriptRun(w, r, false)
}
func (h *hub) handleScriptApply(w http.ResponseWriter, r *http.Request) {
	h.handleScriptRun(w, r, true)
}
func (h *hub) handleScriptRun(w http.ResponseWriter, r *http.Request, apply bool) {
	actor := operatorActor(r)
	action := store.AuditScriptPreview
	if apply {
		action = store.AuditScriptApply
	}
	if !h.requireAuditedJSON(w, r, actor, action) {
		return
	}
	var body store.ScriptRunRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, action, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if e, ok := scriptcatalog.Lookup(body.ScriptID); ok && e.Mode == "write" {
		p, ok := operatorauth.PrincipalFromContext(r.Context())
		if !ok || !p.Has(operatorauth.Admin) {
			h.rejectOperatorPolicyTransport(w, r, actor, action, 403, "SCRIPT_SCOPE_REQUIRED", "write scripts require admin")
			return
		}
		actor.AuthCapability = p.GrantedCapabilities.Admin
	}
	result, err := operator.New(h.store).ScriptRun(body, apply, r.Header.Get("Idempotency-Key"), actor)
	if err != nil {
		writeSettingErr(w, "script run", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if apply {
		writeDiskCleanResult(w, result.Replayed, false, result)
	} else {
		writeJSON(w, 200, result)
	}
}

// Catalog mode, never a caller-supplied mode, selects the boundary scope.
// Buffer only a bounded prefix and replay every byte to the strict handler.
// Unknown or malformed requests retain the phase-one operate requirement.
type scriptReplayBody struct {
	io.Reader
	io.Closer
}

func scriptRequestPermission(r *http.Request) operatorauth.Permission {
	if r.Body == nil {
		return operatorauth.Operate
	}
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, 64<<10+1))
	r.Body = scriptReplayBody{Reader: io.MultiReader(bytes.NewReader(prefix), original), Closer: original}
	if err != nil || len(prefix) > 64<<10 {
		return operatorauth.Admin
	}
	var id struct {
		ScriptID string `json:"script_id"`
	}
	if json.Unmarshal(prefix, &id) != nil {
		return operatorauth.Operate
	}
	if e, ok := scriptcatalog.Lookup(id.ScriptID); ok && e.Mode == "write" {
		return operatorauth.Admin
	}
	return operatorauth.Operate
}
