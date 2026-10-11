package operatoragent

import (
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

// deploymentTimeoutMaxSeconds is the upper bound Hub enforces in
// normalizeDeploymentCreatePreviewRequest. Zero means the server default
// (operator.DefaultDeploymentTimeout, 600). Hub does not export this bound
// as a named constant.
const deploymentTimeoutMaxSeconds = 86400

const (
	idPattern     = `^(?!\.$)(?!\.\.$)[^/\s](?:[^/]*[^/\s])?$`
	sha256Pattern = `^[0-9a-f]{64}$`
	digestPattern = `^sha256:[0-9a-f]{64}$`
)

func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func strEnum(values ...string) map[string]any {
	items := make([]any, len(values))
	for i, value := range values {
		items[i] = value
	}
	return map[string]any{"type": "string", "enum": items}
}

func strField(minLen, maxLen int, pattern, description string) map[string]any {
	field := map[string]any{"type": "string"}
	if minLen > 0 {
		field["minLength"] = minLen
	}
	if maxLen > 0 {
		field["maxLength"] = maxLen
	}
	if pattern != "" {
		field["pattern"] = pattern
	}
	if description != "" {
		field["description"] = description
	}
	return field
}

func intBound(min, max int, description string) map[string]any {
	field := map[string]any{"type": "integer", "minimum": min, "maximum": max}
	if description != "" {
		field["description"] = description
	}
	return field
}

func intMin(min int, description string) map[string]any {
	field := map[string]any{"type": "integer", "minimum": min}
	if description != "" {
		field["description"] = description
	}
	return field
}

func stateArray(values []string) map[string]any {
	return map[string]any{
		"type":        "array",
		"uniqueItems": true,
		"maxItems":    len(values),
		"items":       strEnum(values...),
	}
}

func asStrings[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}

func annotations(title string, readOnly, destructive, idempotent bool) *ToolAnnotations {
	return &ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    readOnly,
		DestructiveHint: destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   false,
	}
}

func readAnn(title string) *ToolAnnotations { return annotations(title, true, false, true) }

func writeAnn(title string, destructive bool) *ToolAnnotations {
	return annotations(title, false, destructive, true)
}

// Tools is the MCP and CLI catalog. Limits come from the Hub validators.
// They are not tighter than those checks. Optional filters omit the field
// for the Hub default; an empty string is not part of a closed enum.
func Tools() []Tool {
	machineStates := asStrings(state.AllStates)
	jobStates := asStrings(deploy.AllJobStates)
	deploymentStates := []string{store.DeploymentRunning, store.DeploymentPaused, store.DeploymentFinished}
	id256 := strField(1, 256, idPattern, `Single path segment, 1 to 256 bytes, no surrounding space, not "." or "..".`)
	version := strField(1, 128, idPattern, `Version identifier, 1 to 128 bytes, no surrounding space, not "." or "..".`)
	cursor := strField(0, 2048, "", "Opaque cursor, at most 2048 bytes, no surrounding space. Omit for the first page.")
	text256 := strField(0, 256, "", "Optional filter. When set: no surrounding space, valid UTF-8, at most 256 bytes.")
	artifact := strField(64, 64, sha256Pattern, "64 lowercase hex characters, no sha256: prefix.")
	digest := "sha256: plus 64 lowercase hex from a prior preview response. This call does not create a preview."
	digestField := strField(71, 71, digestPattern, digest)
	idem := "Caller-chosen idempotency key, 1 to 200 bytes, no surrounding space and no control characters. Reuse it to retry the same write."
	idemField := strField(1, 200, "", idem)
	reason := strField(0, 500, "", "Text up to 500 bytes, no surrounding space. Write tools reject an empty reason.")
	channelCreate := strEnum("canary", "stable")
	channelList := strEnum("canary", "stable")
	machineLimit := intBound(0, operator.MaxMachineReadLimit, "0 uses the server default of 50. Hub accepts 1 to 100.")
	jobLimit := intBound(0, operator.MaxJobReadLimit, "0 uses the server default of 50. Hub accepts 1 to 100.")
	deploymentLimit := intBound(0, operator.MaxDeploymentReadLimit, "0 uses the server default of 50. Hub accepts 1 to 100.")
	machineEvidenceLimit := intBound(0, store.MaxMachineEvidencePageSize, "0 uses the server default. Hub accepts 0 to 100.")
	jobEvidenceLimit := intBound(0, operator.JobEvidenceDefaultLimit, "0 uses the server default. Hub accepts 0 to 100.")
	rolloutBatch := intBound(0, 1, "0 or omitted means 1. rollout_preview and rollout_apply reject any other value before HTTP. deployment_create allows 1 to 5.")
	createBatch := intBound(0, store.MaxDeploymentBatchSize, "0 means the server default of 1. Hub accepts 1 to 5.")
	timeout := intBound(0, deploymentTimeoutMaxSeconds, "0 means the server default of 600 seconds. Hub accepts 1 to 86400.")
	controlRevision := intMin(0, "Hub accepts 0 up to, but not including, the maximum int64 control revision. JSON Schema cannot state that maximum exactly.")
	openedBatch := intMin(1, "Opened batch from the deployment you just read. Hub does not publish an upper bound.")
	profileRevision := intMin(1, "Hub rejects a revision less than or equal to 0. No catalog maximum is applied here.")
	boolField := map[string]any{"type": "boolean"}
	profileID := strField(1, 0, "", "Non-empty profile id. Hub rejects an empty id and does not publish a catalog maximum.")
	enrollName := strField(1, 0, "", "Non-empty display name, no surrounding space. Hub does not publish a maximum for enrollment display names.")
	confirmName := strField(1, 256, "", "Must match the machine display name. At most 256 bytes, no surrounding space.")
	ttl := intBound(int(store.OperatorEnrollTokenMinTTLSeconds), int(store.OperatorEnrollTokenMaxTTLSeconds), "Seconds. Hub accepts 60 to 86400.")

	return append([]Tool{
		{Name: "jobs_summary", Description: "Read bounded Hub job counts and threshold flags. Scope view. Null flags mean insufficient evidence.", InputSchema: obj(map[string]any{"kind": strField(1, 256, "", "Executor kind"), "machine_id": strField(1, 256, "", "Machine identifier"), "window": strEnum("24h", "7d"), "per_machine": boolField}), Annotations: readAnn("jobs_summary")},
		{Name: "approvals_summary", Description: "Read observed approvals. Scope view. Preview and pending metrics are null when not persisted.", InputSchema: obj(map[string]any{"window": strEnum("24h", "7d")}), Annotations: readAnn("approvals_summary")},
		{Name: "hub_status", Description: "Read Hub storage, uptime, restore drill and machine health. Scope view. No secrets or local paths.", InputSchema: obj(map[string]any{}), Annotations: readAnn("hub_status")},
		{Name: "fleet_overview", Description: "Read GET /v1/operator/machines. Totals are the fleet index. Items are one page. A non-null next_cursor means this page is not the whole fleet.", InputSchema: obj(map[string]any{}), Annotations: readAnn("fleet_overview")},
		{Name: "machines_list", Description: "Read GET /v1/operator/machines with the same filters as the operator client.", InputSchema: obj(map[string]any{
			"machine_id": text256, "display_name": text256, "states": stateArray(machineStates),
			"lifecycle": strEnum("any", "active", "retired"), "reporting": strEnum("any", "true", "false", "unknown"),
			"channel": strEnum("any", "none", "canary", "stable"), "limit": machineLimit, "cursor": cursor,
		}), Annotations: readAnn("machines_list")},
		{Name: "machine_get", Description: "Read GET /v1/operator/machines/{id}. Reachability is Hub's judgement from outbound check-ins.", InputSchema: obj(map[string]any{"machine_id": id256}, "machine_id"), Annotations: readAnn("machine_get")},
		{Name: "machine_evidence", Description: "Read GET /v1/operator/machines/{id}/evidence. limit 0 uses the server default.", InputSchema: obj(map[string]any{"machine_id": id256, "limit": machineEvidenceLimit}, "machine_id"), Annotations: readAnn("machine_evidence")},
		{Name: "jobs_list", Description: "Read GET /v1/operator/jobs.", InputSchema: obj(map[string]any{
			"machine_id":    map[string]any{"type": "string", "description": "Optional. When set, Hub rejects surrounding whitespace. No maximum length is published for this filter."},
			"states":        stateArray(jobStates),
			"deployment_id": map[string]any{"type": "string", "description": "Optional. When set, Hub rejects surrounding whitespace. No maximum length is published for this filter."},
			"resource_kind": map[string]any{"type": "string", "description": "When resource_id is set, Hub also requires resource_kind. No maximum length is published."},
			"resource_id":   map[string]any{"type": "string", "description": "Hub requires resource_kind when this is set. No maximum length is published."},
			"limit":         jobLimit, "cursor": cursor,
		}), Annotations: readAnn("jobs_list")},
		{Name: "job_get", Description: "Read GET /v1/operator/jobs/{id}.", InputSchema: obj(map[string]any{"job_id": id256}, "job_id"), Annotations: readAnn("job_get")},
		{Name: "job_evidence", Description: "Read GET /v1/operator/jobs/{id}/evidence. The agent's own status word is not in this tool.", InputSchema: obj(map[string]any{"job_id": id256, "limit": jobEvidenceLimit}, "job_id"), Annotations: readAnn("job_evidence")},
		{Name: "deployments_list", Description: "Read GET /v1/operator/deployments.", InputSchema: obj(map[string]any{
			"channel": channelList, "states": stateArray(deploymentStates), "stuck": boolField, "limit": deploymentLimit, "cursor": cursor,
		}), Annotations: readAnn("deployments_list")},
		{Name: "deployment_get", Description: "Read GET /v1/operator/deployments/{id}, including per-target Hub job state and the cross-failure-domain verdict.", InputSchema: obj(map[string]any{"deployment_id": id256}, "deployment_id"), Annotations: readAnn("deployment_get")},
		{Name: "software_report", Description: "Read GET /v1/operator/software-report.", InputSchema: obj(map[string]any{}), Annotations: readAnn("software_report")},
		{Name: "compliance", Description: "Read GET /v1/operator/compliance.", InputSchema: obj(map[string]any{}), Annotations: readAnn("compliance")},
		{Name: "rollout_status", Description: "Read one deployment and return the shared canary assessment. This tool does not open a batch.", InputSchema: obj(map[string]any{"deployment_id": id256}, "deployment_id"), Annotations: readAnn("rollout_status")},
		{Name: "rollout_preview", Description: "POST /v1/operator/deployments/preview with batch_size 1. Returns the server preview digest and the canary assessment of that plan. Omit batch_size or set it to 1.", InputSchema: obj(map[string]any{
			"channel": channelCreate, "version": version, "artifact_sha256": artifact, "batch_size": rolloutBatch,
			"execution_timeout_seconds": timeout, "irreversible": boolField,
		}, "channel", "version", "artifact_sha256"), Annotations: readAnn("rollout_preview")},
		{Name: "rollout_apply", Description: "POST /v1/operator/deployments for a batch_size 1 plan. Requires preview_digest from rollout_preview or deployment_create_preview. Does not call preview itself.", InputSchema: obj(map[string]any{
			"channel": channelCreate, "version": version, "artifact_sha256": artifact, "batch_size": rolloutBatch,
			"execution_timeout_seconds": timeout, "irreversible": boolField,
			"preview_digest":  digestField,
			"confirm_channel": channelCreate, "confirm_version": version, "reason": reason,
			"idempotency_key": idemField,
		}, "channel", "version", "artifact_sha256", "preview_digest", "confirm_channel", "confirm_version", "reason", "idempotency_key"), Annotations: writeAnn("rollout_apply", true)},
		{Name: "rollout_expand", Description: "Continue a paused deployment only when the shared assessment says the one canary job is succeeded. Requires preview_digest from deployment_continue_preview plus the expected control revision and opened batch from rollout_status. Refuses without calling Hub when the canary is not succeeded. A new deployment pauses after that verdict; this tool does not open the next batch while the deployment is still running. Deployments created before the hold still let the Hub driver open the next batch.", InputSchema: obj(map[string]any{
			"deployment_id":             id256,
			"preview_digest":            digestField,
			"expected_control_revision": controlRevision, "expected_opened_batch": openedBatch,
			"confirm_channel": channelCreate, "reason": reason, "idempotency_key": idemField,
		}, "deployment_id"), Annotations: writeAnn("rollout_expand", true)},
		{Name: "enroll_ticket_preview", Description: "POST /v1/operator/enrollment-tokens/preview. The response preview_digest is required by enroll_ticket_create.", InputSchema: obj(map[string]any{"display_name": enrollName, "ttl_seconds": ttl}, "display_name", "ttl_seconds"), Annotations: readAnn("enroll_ticket_preview")},
		{Name: "enroll_ticket_create", Description: "POST /v1/operator/enrollment-tokens. Requires preview_digest from enroll_ticket_preview. The one-time token is only in a fresh response.", InputSchema: obj(map[string]any{
			"display_name": enrollName, "ttl_seconds": ttl, "preview_digest": digestField, "reason": reason, "idempotency_key": idemField,
		}, "display_name", "ttl_seconds", "preview_digest", "reason", "idempotency_key"), Annotations: writeAnn("enroll_ticket_create", false)},
		{Name: "deployment_create_preview", Description: "POST /v1/operator/deployments/preview. Same route as the deployment form.", InputSchema: obj(map[string]any{
			"channel": channelCreate, "version": version, "artifact_sha256": artifact, "batch_size": createBatch,
			"execution_timeout_seconds": timeout, "irreversible": boolField,
		}, "channel", "version", "artifact_sha256"), Annotations: readAnn("deployment_create_preview")},
		{Name: "deployment_create", Description: "POST /v1/operator/deployments. Requires preview_digest from deployment_create_preview. confirm_channel and confirm_version must match the preview inputs.", InputSchema: obj(map[string]any{
			"channel": channelCreate, "version": version, "artifact_sha256": artifact, "batch_size": createBatch,
			"execution_timeout_seconds": timeout, "irreversible": boolField,
			"preview_digest": digestField, "confirm_channel": channelCreate, "confirm_version": version, "reason": reason, "idempotency_key": idemField,
		}, "channel", "version", "artifact_sha256", "preview_digest", "confirm_channel", "confirm_version", "reason", "idempotency_key"), Annotations: writeAnn("deployment_create", true)},
		{Name: "deployment_continue_preview", Description: "POST /v1/operator/deployments/{id}/continue-preview.", InputSchema: obj(map[string]any{"deployment_id": id256}, "deployment_id"), Annotations: readAnn("deployment_continue_preview")},
		{Name: "deployment_continue", Description: "POST the existing Continue route. Requires preview_digest, expected_control_revision, and expected_opened_batch. Plain Continue refuses a failed batch. This tool does not send skip failed batch. Use rollout_expand after a succeeded canary.", InputSchema: obj(map[string]any{
			"deployment_id": id256, "preview_digest": digestField, "expected_control_revision": controlRevision, "expected_opened_batch": openedBatch,
			"confirm_channel": channelCreate, "reason": reason, "idempotency_key": idemField,
		}, "deployment_id", "preview_digest", "expected_control_revision", "expected_opened_batch", "confirm_channel", "reason", "idempotency_key"), Annotations: writeAnn("deployment_continue", true)},
		{Name: "deployment_abandon_preview", Description: "POST /v1/operator/deployments/{id}/abandon-preview. Abandon stops further jobs. It does not uninstall a succeeded machine.", InputSchema: obj(map[string]any{"deployment_id": id256}, "deployment_id"), Annotations: readAnn("deployment_abandon_preview")},
		{Name: "deployment_abandon", Description: "POST the existing Abandon route. Requires preview_digest, expected_control_revision, and expected_opened_batch.", InputSchema: obj(map[string]any{
			"deployment_id": id256, "preview_digest": digestField, "expected_control_revision": controlRevision, "expected_opened_batch": openedBatch,
			"confirm_deployment_id": id256, "reason": reason, "idempotency_key": idemField,
		}, "deployment_id", "preview_digest", "expected_control_revision", "expected_opened_batch", "confirm_deployment_id", "reason", "idempotency_key"), Annotations: writeAnn("deployment_abandon", true)},
		{Name: "profile_assignment_preview", Description: "POST /v1/operator/machines/{id}/profile-assignment-preview.", InputSchema: obj(map[string]any{
			"machine_id": id256, "profile_id": profileID, "profile_revision": profileRevision,
		}, "machine_id", "profile_id", "profile_revision"), Annotations: readAnn("profile_assignment_preview")},
		{Name: "profile_assignment_apply", Description: "POST /v1/operator/machines/{id}/profile-assignments. Requires preview_digest from profile_assignment_preview.", InputSchema: obj(map[string]any{
			"machine_id": id256, "profile_id": profileID, "profile_revision": profileRevision, "confirm_display_name": confirmName,
			"preview_digest": digestField, "reason": reason, "idempotency_key": idemField,
		}, "machine_id", "profile_id", "profile_revision", "confirm_display_name", "preview_digest", "reason", "idempotency_key"), Annotations: writeAnn("profile_assignment_apply", true)},
	}, diskCleanTools(id256, idemField)...)
}
