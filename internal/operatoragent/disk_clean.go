package operatoragent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

// diskCleanTools is the disk-clean part of the catalog. id and idem are the
// shared id256 and idempotency-key schemas from Tools.
func diskCleanTools(id, idem map[string]any) []Tool {
	digest := strField(71, 71, digestPattern, "sha256: plus 64 lowercase hex from the matching disk-clean preview. This call does not create a preview.")
	scopeType := strEnum("machine", "channel")
	scopeID := strField(1, 256, idPattern, "machine id, or canary or stable when scope_type is channel")
	machines := map[string]any{
		"type": "array", "minItems": 1, "maxItems": 64, "uniqueItems": true,
		"items": strField(1, 128, idPattern, ""),
	}
	reason := strField(1, 500, "", "Text up to 500 bytes, no surrounding space. Required.")
	revision := intMin(1, "Disk-clean profile revision for this scope.")
	expectedRevision := intMin(0, "current_revision from the profile preview; 0 when the scope has no profile yet.")
	controlRevision := intMin(1, "control_revision from the matching rollout preview.")
	openedBatch := intBound(1, 2, "opened_batch from the matching rollout preview.")
	profile := diskCleanProfileSchema()
	return []Tool{
		{Name: "disk_clean_summaries", Description: "Read GET /v1/operator/disk-clean/summaries. Hub verdict, staleness, digest match, disk_free_min_percent, and attention. Empty when nothing is assigned and nothing has reported.", InputSchema: obj(map[string]any{}), Annotations: readAnn("disk_clean_summaries")},
		{Name: "disk_clean_summary", Description: "Read GET /v1/operator/disk-clean/summaries/{id}. An enrolled machine with no assignment is stale, not missing.", InputSchema: obj(map[string]any{"machine_id": id}, "machine_id"), Annotations: readAnn("disk_clean_summary")},
		{Name: "disk_clean_profile_preview", Description: "POST /v1/operator/disk-clean/profile-preview. Renders the conf and returns preview_digest and config_digest. Does not mint a revision.", InputSchema: obj(map[string]any{
			"scope_type": scopeType, "scope_id": scopeID, "profile": profile,
		}, "scope_type", "scope_id", "profile"), Annotations: readAnn("disk_clean_profile_preview")},
		{Name: "disk_clean_profile_publish", Description: "POST /v1/operator/disk-clean/profiles. Requires preview_digest from disk_clean_profile_preview and expected_revision from that preview's current_revision. Same rendered conf does not mint a new revision.", InputSchema: obj(map[string]any{
			"scope_type": scopeType, "scope_id": scopeID, "profile": profile,
			"expected_revision": expectedRevision, "preview_digest": digest,
			"confirm_scope_id": scopeID, "reason": reason, "idempotency_key": idem,
		}, "scope_type", "scope_id", "profile", "expected_revision", "preview_digest", "confirm_scope_id", "reason", "idempotency_key"), Annotations: writeAnn("disk_clean_profile_publish", false)},
		{Name: "disk_clean_dry_run_preview", Description: "POST /v1/operator/disk-clean/dry-run-preview. Names the machines that would receive a reversible disk-clean job. Blockers are not part of the digest.", InputSchema: obj(map[string]any{
			"scope_type": scopeType, "scope_id": scopeID, "revision": revision, "machine_ids": machines,
		}, "scope_type", "scope_id", "revision", "machine_ids"), Annotations: readAnn("disk_clean_dry_run_preview")},
		{Name: "disk_clean_dry_run_apply", Description: "POST /v1/operator/disk-clean/dry-runs. Creates one dry-run job per machine. Requires preview_digest from disk_clean_dry_run_preview. Does not delete files.", InputSchema: obj(map[string]any{
			"scope_type": scopeType, "scope_id": scopeID, "revision": revision, "machine_ids": machines,
			"preview_digest": digest, "confirm_scope_id": scopeID, "reason": reason, "idempotency_key": idem,
		}, "scope_type", "scope_id", "revision", "machine_ids", "preview_digest", "confirm_scope_id", "reason", "idempotency_key"), Annotations: writeAnn("disk_clean_dry_run_apply", false)},
		{Name: "disk_clean_canary_preview", Description: "POST /v1/operator/disk-clean/canary-preview. The canary is exactly one machine from machine_ids. The rest stay closed until continue.", InputSchema: obj(map[string]any{
			"scope_type": scopeType, "scope_id": scopeID, "revision": revision, "machine_ids": machines, "canary_machine_id": id,
		}, "scope_type", "scope_id", "revision", "machine_ids", "canary_machine_id"), Annotations: readAnn("disk_clean_canary_preview")},
		{Name: "disk_clean_canary_apply", Description: "POST /v1/operator/disk-clean/canaries. Opens a rollout whose first batch is exactly the canary. Requires a succeeded dry-run of this revision on every target, and preview_digest from disk_clean_canary_preview.", InputSchema: obj(map[string]any{
			"scope_type": scopeType, "scope_id": scopeID, "revision": revision, "machine_ids": machines, "canary_machine_id": id,
			"preview_digest": digest, "confirm_scope_id": scopeID, "reason": reason, "idempotency_key": idem,
		}, "scope_type", "scope_id", "revision", "machine_ids", "canary_machine_id", "preview_digest", "confirm_scope_id", "reason", "idempotency_key"), Annotations: writeAnn("disk_clean_canary_apply", true)},
		{Name: "disk_clean_continue_preview", Description: "POST /v1/operator/disk-clean/continuation-preview. Digest includes the rollout state, so a reconcile between preview and apply is stale.", InputSchema: obj(map[string]any{"rollout_id": id}, "rollout_id"), Annotations: readAnn("disk_clean_continue_preview")},
		{Name: "disk_clean_continue_apply", Description: "POST /v1/operator/disk-clean/continuations. Opens the rest only after the canary succeeded and the rollout is paused. Requires preview_digest, expected_control_revision, and expected_opened_batch.", InputSchema: obj(map[string]any{
			"rollout_id": id, "preview_digest": digest,
			"expected_control_revision": controlRevision, "expected_opened_batch": openedBatch,
			"confirm_rollout_id": id, "reason": reason, "idempotency_key": idem,
		}, "rollout_id", "preview_digest", "expected_control_revision", "expected_opened_batch", "confirm_rollout_id", "reason", "idempotency_key"), Annotations: writeAnn("disk_clean_continue_apply", true)},
		{Name: "disk_clean_abandon_preview", Description: "POST /v1/operator/disk-clean/abandonment-preview. Abandon opens no further jobs.", InputSchema: obj(map[string]any{"rollout_id": id}, "rollout_id"), Annotations: readAnn("disk_clean_abandon_preview")},
		{Name: "disk_clean_abandon_apply", Description: "POST /v1/operator/disk-clean/abandonments. Stops the rollout. Refuses while a disk-clean job is still running. Requires preview_digest, expected_control_revision, and expected_opened_batch.", InputSchema: obj(map[string]any{
			"rollout_id": id, "preview_digest": digest,
			"expected_control_revision": controlRevision, "expected_opened_batch": openedBatch,
			"confirm_rollout_id": id, "reason": reason, "idempotency_key": idem,
		}, "rollout_id", "preview_digest", "expected_control_revision", "expected_opened_batch", "confirm_rollout_id", "reason", "idempotency_key"), Annotations: writeAnn("disk_clean_abandon_apply", true)},
	}
}

func diskCleanProfileSchema() map[string]any {
	categories := []string{
		"user_tmp", "tmp_globs", "npm_cache", "pip_cache", "uv_cache", "go_cache", "thumbnails", "trash",
		"tmpfiles", "journal", "pkg_cache", "docker", "snap",
	}
	bounded := func(min, max int) map[string]any {
		return map[string]any{"type": "integer", "minimum": min, "maximum": max}
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "Closed disk-clean profile. User scope uses the user categories and may set tmp_dirs, cache floors, and report paths. Root scope uses the root categories. Protected names are not a field: the Hub always renders the built-in list, and extra_protect_names can only add.",
		"required":    []string{"schema_version", "scope", "dry_run", "categories", "tmp_age_days", "attention_pct", "mount"},
		"properties": map[string]any{
			"schema_version": map[string]any{"type": "integer", "enum": []int{maintenance.SchemaVersion}},
			"scope":          map[string]any{"type": "string", "enum": []string{maintenance.ScopeUser, maintenance.ScopeRoot}},
			"dry_run":        map[string]any{"type": "boolean"},
			"categories": map[string]any{
				"type": "array", "minItems": 1, "maxItems": len(categories), "uniqueItems": true,
				"items":       map[string]any{"type": "string", "enum": categories},
				"description": "User scope: user_tmp tmp_globs npm_cache pip_cache uv_cache go_cache thumbnails trash. Root scope: tmpfiles journal pkg_cache docker snap. docker prunes dangling images and build cache only.",
			},
			"apply_categories": map[string]any{
				"type": "array", "maxItems": len(categories), "uniqueItems": true,
				"items":       map[string]any{"type": "string", "enum": categories},
				"description": "Subset of categories. Only valid when dry_run is true.",
			},
			"tmp_dirs": map[string]any{
				"type": "array", "maxItems": 2, "uniqueItems": true,
				"items":       map[string]any{"type": "string", "enum": []string{"/tmp", "/var/tmp"}},
				"description": "User scope only. Any non-empty subset of /tmp and /var/tmp.",
			},
			"tmp_age_days":       bounded(maintenance.MinTmpAgeDays, maintenance.MaxTmpAgeDays),
			"npm_clean_min_mb":   bounded(maintenance.MinCacheMB, maintenance.MaxCacheMB),
			"pip_cache_min_mb":   bounded(maintenance.MinCacheMB, maintenance.MaxCacheMB),
			"go_cache_min_mb":    bounded(maintenance.MinCacheMB, maintenance.MaxCacheMB),
			"thumb_age_days":     bounded(maintenance.MinTmpAgeDays, maintenance.MaxTmpAgeDays),
			"trash_age_days":     bounded(maintenance.MinTmpAgeDays, maintenance.MaxTmpAgeDays),
			"du_timeout_s":       bounded(maintenance.MinDuTimeoutS, maintenance.MaxDuTimeoutS),
			"du_depth":           bounded(maintenance.MinDuDepth, maintenance.MaxDuDepth),
			"vartmp_age_days":    bounded(maintenance.MinTmpAgeDays, maintenance.MaxTmpAgeDays),
			"journal_max_size":   map[string]any{"type": "string", "pattern": "^[1-9][0-9]{0,5}[KMGT]$"},
			"journal_max_age":    map[string]any{"type": "string", "pattern": "^[1-9][0-9]{0,3}d$"},
			"docker_until_hours": bounded(maintenance.MinDockerHours, maintenance.MaxDockerHours),
			"attention_pct":      bounded(maintenance.MinAttentionPct, maintenance.MaxAttentionPct),
			"mount":              map[string]any{"type": "string", "const": "/"},
			"extra_protect_names": map[string]any{
				"type": "array", "maxItems": maintenance.MaxExtraProtect, "uniqueItems": true,
				"items": map[string]any{"type": "string", "minLength": 1, "maxLength": maintenance.MaxTokenLen, "pattern": "^[A-Za-z0-9.*_?-]+$"},
			},
			"tmp_glob_rules": map[string]any{
				"type": "array", "maxItems": maintenance.MaxGlobRules,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []string{"glob", "min_age_days", "keep_newest"},
					"properties": map[string]any{
						"glob":         map[string]any{"type": "string", "pattern": "^/(tmp|var/tmp)/[A-Za-z0-9.*_?-]+$"},
						"min_age_days": bounded(maintenance.MinTmpAgeDays, maintenance.MaxTmpAgeDays),
						"keep_newest":  bounded(maintenance.MinKeepNewest, maintenance.MaxKeepNewest),
						"busy_regex":   map[string]any{"type": "string", "maxLength": maintenance.MaxBusyRegex},
					},
				},
			},
			"report_paths": map[string]any{
				"type": "array", "maxItems": maintenance.MaxReportPaths,
				"items": map[string]any{"type": "string", "pattern": "^\\$HOME(/[A-Za-z0-9._*-]+)+$"},
			},
		},
	}
}

func (s *Service) diskCleanSummaries(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	items, err := s.Hub.DiskCleanSummaries(ctx)
	if err != nil {
		return nil, hubErr(err)
	}
	return map[string]any{"source": "GET /v1/operator/disk-clean/summaries", "items": items}, nil
}

func (s *Service) diskCleanSummary(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID string `json:"machine_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireID("machine_id", args.MachineID); err != nil {
		return nil, err
	}
	view, err := s.Hub.DiskCleanSummary(ctx, args.MachineID)
	if err != nil {
		return nil, hubErr(err)
	}
	return view, nil
}

func (s *Service) diskCleanProfilePreview(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ScopeType string              `json:"scope_type"`
		ScopeID   string              `json:"scope_id"`
		Profile   maintenance.Profile `json:"profile"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireDiskCleanScopeArgs(args.ScopeType, args.ScopeID); err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDiskCleanProfile(ctx, operatorclient.DiskCleanProfilePreviewRequest{
		ScopeType: args.ScopeType, ScopeID: args.ScopeID, Profile: args.Profile,
	})
	return hubValue(result, err)
}

func (s *Service) diskCleanProfilePublish(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ScopeType        string              `json:"scope_type"`
		ScopeID          string              `json:"scope_id"`
		Profile          maintenance.Profile `json:"profile"`
		ExpectedRevision *int64              `json:"expected_revision"`
		PreviewDigest    string              `json:"preview_digest"`
		ConfirmScopeID   string              `json:"confirm_scope_id"`
		Reason           string              `json:"reason"`
		IdempotencyKey   string              `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return nil, err
	}
	if args.ExpectedRevision == nil {
		return nil, &CallError{Code: "expected_revision_required", Message: "expected_revision is required"}
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireDiskCleanScopeArgs(args.ScopeType, args.ScopeID); err != nil {
		return nil, err
	}
	if err := requireDiskCleanReason(args.Reason, args.ConfirmScopeID, args.ScopeID); err != nil {
		return nil, err
	}
	result, err := s.Hub.PublishDiskCleanProfile(ctx, args.IdempotencyKey, operatorclient.DiskCleanProfilePublishRequest{
		ScopeType: args.ScopeType, ScopeID: args.ScopeID, Profile: args.Profile,
		ExpectedRevision: args.ExpectedRevision, PreviewDigest: args.PreviewDigest,
		ConfirmScopeID: args.ConfirmScopeID, Reason: args.Reason,
	})
	return hubValue(result, err)
}

func (s *Service) diskCleanDryRunPreview(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeDiskCleanTargets(raw, false)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDiskCleanDryRun(ctx, operatorclient.DiskCleanTargetPreviewRequest{
		ScopeType: args.ScopeType, ScopeID: args.ScopeID, Revision: args.Revision, MachineIDs: args.MachineIDs,
	})
	return hubValue(result, err)
}

func (s *Service) diskCleanDryRunApply(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeDiskCleanTargets(raw, true)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.ApplyDiskCleanDryRun(ctx, args.IdempotencyKey, operatorclient.DiskCleanTargetApplyRequest{
		ScopeType: args.ScopeType, ScopeID: args.ScopeID, Revision: args.Revision, MachineIDs: args.MachineIDs,
		PreviewDigest: args.PreviewDigest, ConfirmScopeID: args.ConfirmScopeID, Reason: args.Reason,
	})
	return hubValue(result, err)
}

func (s *Service) diskCleanCanaryPreview(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeDiskCleanTargets(raw, false)
	if err != nil {
		return nil, err
	}
	if err := requireDiskCleanCanary(args.CanaryMachineID, args.MachineIDs); err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDiskCleanCanary(ctx, operatorclient.DiskCleanCanaryPreviewRequest{
		ScopeType: args.ScopeType, ScopeID: args.ScopeID, Revision: args.Revision,
		MachineIDs: args.MachineIDs, CanaryMachineID: args.CanaryMachineID,
	})
	return hubValue(result, err)
}

func (s *Service) diskCleanCanaryApply(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeDiskCleanTargets(raw, true)
	if err != nil {
		return nil, err
	}
	if err := requireDiskCleanCanary(args.CanaryMachineID, args.MachineIDs); err != nil {
		return nil, err
	}
	result, err := s.Hub.ApplyDiskCleanCanary(ctx, args.IdempotencyKey, operatorclient.DiskCleanCanaryApplyRequest{
		ScopeType: args.ScopeType, ScopeID: args.ScopeID, Revision: args.Revision,
		MachineIDs: args.MachineIDs, CanaryMachineID: args.CanaryMachineID,
		PreviewDigest: args.PreviewDigest, ConfirmScopeID: args.ConfirmScopeID, Reason: args.Reason,
	})
	return hubValue(result, err)
}

func (s *Service) diskCleanContinuePreview(ctx context.Context, raw json.RawMessage) (any, error) {
	id, err := decodeDiskCleanRolloutID(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDiskCleanContinue(ctx, id)
	return hubValue(result, err)
}

func (s *Service) diskCleanAbandonPreview(ctx context.Context, raw json.RawMessage) (any, error) {
	id, err := decodeDiskCleanRolloutID(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDiskCleanAbandon(ctx, id)
	return hubValue(result, err)
}

func (s *Service) diskCleanContinueApply(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeDiskCleanControl(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.ContinueDiskClean(ctx, args.IdempotencyKey, args.body)
	return hubValue(result, err)
}

func (s *Service) diskCleanAbandonApply(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeDiskCleanControl(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.AbandonDiskClean(ctx, args.IdempotencyKey, args.body)
	return hubValue(result, err)
}

func hubValue(result any, err error) (any, error) {
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

type diskCleanTargetArgs struct {
	ScopeType       string   `json:"scope_type"`
	ScopeID         string   `json:"scope_id"`
	Revision        int64    `json:"revision"`
	MachineIDs      []string `json:"machine_ids"`
	CanaryMachineID string   `json:"canary_machine_id"`
	PreviewDigest   string   `json:"preview_digest"`
	ConfirmScopeID  string   `json:"confirm_scope_id"`
	Reason          string   `json:"reason"`
	IdempotencyKey  string   `json:"idempotency_key"`
}

func decodeDiskCleanTargets(raw json.RawMessage, write bool) (diskCleanTargetArgs, error) {
	var args diskCleanTargetArgs
	if err := decodeArgs(raw, &args); err != nil {
		return args, err
	}
	if write {
		if err := requireDigest(args.PreviewDigest); err != nil {
			return args, err
		}
		if err := requireIdempotency(args.IdempotencyKey); err != nil {
			return args, err
		}
	}
	if err := requireDiskCleanScopeArgs(args.ScopeType, args.ScopeID); err != nil {
		return args, err
	}
	if args.Revision <= 0 {
		return args, &CallError{Code: "invalid_arguments", Message: "revision must be greater than zero"}
	}
	if err := requireDiskCleanMachines(args.MachineIDs); err != nil {
		return args, err
	}
	if write {
		if err := requireDiskCleanReason(args.Reason, args.ConfirmScopeID, args.ScopeID); err != nil {
			return args, err
		}
	}
	return args, nil
}

type diskCleanControlCall struct {
	IdempotencyKey string
	body           operatorclient.DiskCleanControlApplyRequest
}

func decodeDiskCleanControl(raw json.RawMessage) (diskCleanControlCall, error) {
	var args struct {
		RolloutID               string `json:"rollout_id"`
		ExpectedControlRevision *int64 `json:"expected_control_revision"`
		ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
		PreviewDigest           string `json:"preview_digest"`
		ConfirmRolloutID        string `json:"confirm_rollout_id"`
		Reason                  string `json:"reason"`
		IdempotencyKey          string `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return diskCleanControlCall{}, err
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return diskCleanControlCall{}, err
	}
	if args.ExpectedControlRevision == nil || args.ExpectedOpenedBatch == nil ||
		*args.ExpectedControlRevision <= 0 || *args.ExpectedOpenedBatch <= 0 {
		return diskCleanControlCall{}, &CallError{Code: "expected_revision_required", Message: "expected_control_revision and expected_opened_batch are required"}
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return diskCleanControlCall{}, err
	}
	if err := requireID("rollout_id", args.RolloutID); err != nil {
		return diskCleanControlCall{}, err
	}
	if strings.TrimSpace(args.Reason) == "" || args.Reason != strings.TrimSpace(args.Reason) || args.ConfirmRolloutID == "" {
		return diskCleanControlCall{}, &CallError{Code: "invalid_arguments", Message: "reason and confirm_rollout_id are required"}
	}
	return diskCleanControlCall{
		IdempotencyKey: args.IdempotencyKey,
		body: operatorclient.DiskCleanControlApplyRequest{
			RolloutID: args.RolloutID, ExpectedControlRevision: *args.ExpectedControlRevision,
			ExpectedOpenedBatch: *args.ExpectedOpenedBatch, PreviewDigest: args.PreviewDigest,
			ConfirmRolloutID: args.ConfirmRolloutID, Reason: args.Reason,
		},
	}, nil
}

func decodeDiskCleanRolloutID(raw json.RawMessage) (string, error) {
	var args struct {
		RolloutID string `json:"rollout_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", err
	}
	if err := requireID("rollout_id", args.RolloutID); err != nil {
		return "", err
	}
	return args.RolloutID, nil
}

func requireDiskCleanScopeArgs(scopeType, scopeID string) error {
	if scopeType != "machine" && scopeType != "channel" {
		return &CallError{Code: "invalid_arguments", Message: "scope_type must be machine or channel"}
	}
	if err := requireID("scope_id", scopeID); err != nil {
		return err
	}
	if scopeType == "channel" && scopeID != "canary" && scopeID != "stable" {
		return &CallError{Code: "invalid_arguments", Message: "channel scope_id must be canary or stable"}
	}
	return nil
}

func requireDiskCleanMachines(ids []string) error {
	if len(ids) == 0 || len(ids) > 64 {
		return &CallError{Code: "invalid_arguments", Message: "machine_ids must contain 1 to 64 ids"}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if err := requireID("machine_id", id); err != nil {
			return err
		}
		if seen[id] {
			return &CallError{Code: "invalid_arguments", Message: "machine_ids contains a duplicate"}
		}
		seen[id] = true
	}
	return nil
}

func requireDiskCleanCanary(canary string, ids []string) error {
	if err := requireID("canary_machine_id", canary); err != nil {
		return err
	}
	for _, id := range ids {
		if id == canary {
			return nil
		}
	}
	return &CallError{Code: "invalid_arguments", Message: "canary_machine_id must be one of machine_ids"}
}

func requireDiskCleanReason(reason, confirm, scopeID string) error {
	if strings.TrimSpace(reason) == "" || reason != strings.TrimSpace(reason) {
		return &CallError{Code: "invalid_arguments", Message: "reason is required"}
	}
	if confirm == "" {
		return &CallError{Code: "invalid_arguments", Message: "confirm_scope_id is required"}
	}
	if confirm != scopeID {
		return &CallError{Code: "invalid_arguments", Message: "confirm_scope_id must match scope_id"}
	}
	return nil
}
