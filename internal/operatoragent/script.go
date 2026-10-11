package operatoragent

import (
	"context"
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Optional interface keeps existing Hub adapters source compatible.
type scriptHub interface {
	ScriptCatalog(context.Context) ([]scriptcatalog.Entry, error)
	ScriptRun(context.Context, store.ScriptRunRequest, bool, string) (store.ScriptRunResult, error)
}

func scriptTools() []Tool {
	props := map[string]any{
		"script_id":       strField(1, 128, idPattern, "Embedded catalog ID"),
		"script_sha256":   strField(64, 64, "^[a-f0-9]{64}$", "Exact catalog hash"),
		"args":            map[string]any{"type": "object", "description": "Closed per-script schema returned by script_catalog_list"},
		"targets":         map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "uniqueItems": true, "items": strField(1, 128, idPattern, "")},
		"timeout_seconds": intBound(1, 60, "Catalog bounded timeout"), "reason": strField(1, 500, "", "Reason without credentials"),
	}
	apply := map[string]any{}
	for k, v := range props {
		apply[k] = v
	}
	apply["preview_digest"] = strField(71, 71, digestPattern, "From matching script_run_preview")
	apply["idempotency_key"] = strField(1, 200, "", "Idempotency key")
	return []Tool{
		{Name: "script_catalog_list", Description: "List embedded, hash-pinned scripts. Requires view.", InputSchema: obj(map[string]any{}), Annotations: readAnn("script_catalog_list")},
		{Name: "script_run_preview", Description: "Preview explicit targets for an embedded script. Requires operate for read scripts, admin for write scripts.", InputSchema: obj(props, "script_id", "script_sha256", "args", "targets", "reason"), Annotations: readAnn("script_run_preview")},
		{Name: "script_run_apply", Description: "Enqueue catalog jobs using the digest from script_run_preview. Requires operate for read scripts, admin for write scripts.", InputSchema: obj(apply, "script_id", "script_sha256", "args", "targets", "reason", "preview_digest", "idempotency_key"), Annotations: writeAnn("script_run_apply", false)},
	}
}
func (s *Service) scriptCall(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	h, ok := s.Hub.(scriptHub)
	if !ok {
		return nil, &CallError{Code: "not_configured", Message: "script Hub client unavailable"}
	}
	if name == "script_catalog_list" {
		if err := decodeEmpty(raw); err != nil {
			return nil, err
		}
		result, err := h.ScriptCatalog(ctx)
		return result, hubErr(err)
	}
	// Decode only known wire coordinates, including the MCP-only idempotency key.
	var args struct {
		ScriptID       string          `json:"script_id"`
		ScriptSHA256   string          `json:"script_sha256"`
		Args           json.RawMessage `json:"args"`
		Targets        []string        `json:"targets"`
		TimeoutSeconds int             `json:"timeout_seconds"`
		Reason         string          `json:"reason"`
		PreviewDigest  string          `json:"preview_digest"`
		IdempotencyKey string          `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	apply := name == "script_run_apply"
	if !apply && (args.PreviewDigest != "" || args.IdempotencyKey != "") {
		return nil, &CallError{Code: "invalid_arguments", Message: "preview does not accept apply coordinates"}
	}
	if apply {
		if err := requireIdempotency(args.IdempotencyKey); err != nil {
			return nil, err
		}
		if err := requireDigest(args.PreviewDigest); err != nil {
			return nil, err
		}
	}
	body := store.ScriptRunRequest{ScriptID: args.ScriptID, ScriptSHA256: args.ScriptSHA256, Args: args.Args, Targets: args.Targets, TimeoutSeconds: args.TimeoutSeconds, Reason: args.Reason, PreviewDigest: args.PreviewDigest}
	result, err := h.ScriptRun(ctx, body, apply, args.IdempotencyKey)
	return result, hubErr(err)
}
