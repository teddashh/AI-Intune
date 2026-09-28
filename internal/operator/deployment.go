package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/store"
)

// TerminalJobOutcome 是非成功終態對人顯示的結果短語。
//
// ⚠ internal/deploy/deploy.go:106-110 說明 failed 與 manual_intervention 的回退證據相反；
// rejected 與 lease_expired 的機器現況也不同，因此四種終態不能共用一個字。
// 這裡不與 machineTimelineJobOutcome 合併：時間軸包含 succeeded，且 rejected、
// lease_expired 與未知狀態各自使用不同語意。
func TerminalJobOutcome(state deploy.JobState) string {
	switch state {
	case deploy.Failed:
		return "失敗"
	case deploy.Rejected:
		return "被拒絕，機器沒有改動"
	case deploy.LeaseExpired:
		return "代理程式沒有回報，Hub 收了這張單"
	case deploy.ManualIntervention:
		return "需要人介入，沒有回退證據"
	default:
		return "終態未成功"
	}
}

const (
	DeploymentReadSchemaVersion    = 3
	DeploymentPreviewSchemaVersion = 3
	DefaultDeploymentReadLimit     = 50
	MaxDeploymentReadLimit         = 100
	DefaultDeploymentBatchSize     = 5
	DefaultDeploymentTimeout       = 600
	DeploymentReadConsistencyLive  = "live"
)

var (
	ErrInvalidDeploymentRead    = errors.New("operator: invalid deployment read request")
	ErrInvalidDeploymentPreview = errors.New("operator: invalid deployment preview request")
)

var deploymentStates = []string{
	store.DeploymentRunning,
	store.DeploymentPaused,
	store.DeploymentFinished,
}

type DeploymentListRequest struct {
	Channel string
	States  []string
	Stuck   *bool
	Limit   int
	Cursor  string
}

type DeploymentStateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

type DeploymentListResult struct {
	SchemaVersion int                    `json:"schema_version"`
	Consistency   string                 `json:"consistency"`
	EvaluatedAt   time.Time              `json:"evaluated_at"`
	Total         int                    `json:"total"`
	StateCounts   []DeploymentStateCount `json:"state_counts"`
	Items         []DeploymentSummary    `json:"items"`
	NextCursor    *string                `json:"next_cursor"`
}

type DeploymentSummary struct {
	DeploymentID    string                     `json:"deployment_id"`
	Channel         string                     `json:"channel"`
	DesiredID       string                     `json:"desired_id"`
	ResourceKind    string                     `json:"resource_kind"`
	ResourceID      string                     `json:"resource_id"`
	DesiredRevision deploy.Revision            `json:"desired_revision"`
	ControlRevision int64                      `json:"control_revision"`
	BatchSize       int                        `json:"batch_size"`
	State           string                     `json:"state"`
	CreatedAt       time.Time                  `json:"created_at"`
	PausedAt        *time.Time                 `json:"paused_at"`
	FinishedAt      *time.Time                 `json:"finished_at"`
	RetryOf         *string                    `json:"retry_of"`
	Attempt         int                        `json:"attempt"`
	OpenedBatch     int                        `json:"opened_batch"`
	TotalBatches    int                        `json:"total_batches"`
	Stuck           int                        `json:"stuck"`
	TerminalStuck   int                        `json:"terminal_stuck"`
	SilentStuck     int                        `json:"silent_stuck"`
	JobStateCounts  []JobStateCount            `json:"job_state_counts"`
	Material        DeploymentMaterialSummary  `json:"material"`
	BoundaryPause   *DeploymentBoundarySummary `json:"boundary_pause"`
}

type DeploymentMaterialStatus string

const (
	DeploymentMaterialRecorded    DeploymentMaterialStatus = "recorded"
	DeploymentMaterialInvalid     DeploymentMaterialStatus = "invalid"
	DeploymentMaterialUnsupported DeploymentMaterialStatus = "unsupported"
)

type DeploymentMaterialSummary struct {
	Status         DeploymentMaterialStatus `json:"status"`
	Kind           string                   `json:"kind"`
	Version        *string                  `json:"version"`
	ArtifactDigest *string                  `json:"artifact_digest"`
	SizeBytes      *int64                   `json:"size_bytes"`
	EnginesNode    *string                  `json:"engines_node"`
}

type DeploymentBoundarySummary struct {
	OpenedBatch int       `json:"opened_batch"`
	Kind        string    `json:"kind"`
	PausedAt    time.Time `json:"paused_at"`
}

type DeploymentTargetSummary struct {
	MachineID      string           `json:"machine_id"`
	DisplayName    string           `json:"display_name"`
	BatchNo        int              `json:"batch_no"`
	JobID          *string          `json:"job_id"`
	ExcludedReason *string          `json:"excluded_reason"`
	JobState       *deploy.JobState `json:"job_state"`
	JobCreatedAt   *time.Time       `json:"job_created_at"`
	TerminalAt     *time.Time       `json:"terminal_at"`
	LastActivityAt *time.Time       `json:"last_activity_at"`
	StuckKind      *string          `json:"stuck_kind"`
	// Independent is the second producer's verdict for this target's job. It is
	// nil exactly when the target has no job: a target that was never opened has
	// nothing to verify, which is a different fact from an opened job that no
	// verifier has written for. The latter is present with verdict absent.
	Independent *DeploymentTargetIndependent `json:"independent"`
}

// DeploymentTargetIndependent carries one target's independent verdict and the
// weight behind it. Verdict is one of the eight store.IndependentVerdict values
// and is decided by the same precedence the job page uses; six of them are
// neither a pass nor a rule failure.
type DeploymentTargetIndependent struct {
	Verdict       string `json:"verdict"`
	Rows          int    `json:"rows"`
	LiveProducers int    `json:"live_producers"`
}

// DeploymentIndependentVerdictCount is how many of this deployment's opened
// targets reached one verdict.
type DeploymentIndependentVerdictCount struct {
	Verdict string `json:"verdict"`
	Targets int    `json:"targets"`
}

// DeploymentIndependentSummary counts opened targets by independent verdict.
// All eight verdicts are always listed, in contract precedence order, so a reader
// can tell "no target reached this verdict" from "this verdict was not
// evaluated". PassedTargets is not a deployment-wide pass: it says how many
// targets a second producer reported passing, and nothing about the rest.
type DeploymentIndependentSummary struct {
	OpenedTargets int                                 `json:"opened_targets"`
	PassedTargets int                                 `json:"passed_targets"`
	LiveProducers int                                 `json:"live_producers"`
	Verdicts      []DeploymentIndependentVerdictCount `json:"verdicts"`
}

// deploymentIndependentVerdictOrder is the contract precedence order, and the
// order these counts are reported in. It is not sorted by count: a reader
// comparing two deployments needs the same rows in the same places.
var deploymentIndependentVerdictOrder = []store.IndependentVerdict{
	store.IndependentAbsent, store.IndependentProducerRevoked, store.IndependentDigestMismatch,
	store.IndependentReleaseMismatch, store.IndependentStale, store.IndependentFailed,
	store.IndependentReleaseUnreported, store.IndependentPassed,
}

type DeploymentActionEligibility struct {
	Eligible        bool     `json:"eligible"`
	Outcome         string   `json:"outcome"`
	AffectedTargets int      `json:"affected_targets"`
	Blockers        []string `json:"blockers"`
}

// DeploymentBlockerLabel 將 blocker token 轉成給操作員看的說明。
// ⚠ 這些文案放在 operator，是因為 Web 與 CLI 必須共用同一句；不能放進 internal/deploy，因為該 package 宣告自己是純 deployment 邏輯，而且 cmd/clawctl-agent 會 import 它。
func DeploymentBlockerLabel(blocker string) string {
	switch blocker {
	case "deployment_not_paused", "deployment_not_retryable":
		return "deployment 目前狀態不允許這個動作"
	case "nonterminal_jobs":
		return "仍有未終態工作單"
	case "no_terminal_failure_targets":
		return "沒有終態未成功且可 retry 的機器"
	case "stable_promotion_locked":
		return "stable promotion 安全閘門目前未通過"
	case "invalid_material", "material_unavailable", "material_identity_changed":
		return "Hub 無法重新驗證原 deployment material"
	case "active_resource_deployment":
		return "同一資源已有 active deployment"
	case "no_opened_batch":
		return "帳本沒有已開批次"
	case "next_batch_empty", "no_included_targets":
		return "沒有可開啟的 target"
	case "target_snapshot_changed":
		return "target snapshot 已變更"
	case "control_revision_exhausted":
		return "deployment 控制版本已達上限，不能再執行動作"
	default:
		return "目前安全條件不允許這個動作（" + blocker + "）"
	}
}

// DeploymentActionImpact 說明 deployment 動作確認後的影響，或目前不可執行的原因。
func DeploymentActionImpact(action string, eligibility DeploymentActionEligibility) string {
	if action != "continue" && action != "retry" && action != "abandon" {
		return ""
	}
	if eligibility.Eligible {
		switch action {
		case "continue":
			if eligibility.Outcome == "finish" {
				return "所有批次都已開完；確認後會把 deployment 收成 finished，既有終態未成功的工作單不會重開。"
			}
			return fmt.Sprintf("將開下一批 %d 台；既有終態未成功的工作單會保留，不會重開。", eligibility.AffectedTargets)
		case "retry":
			// ⚠ terminal_failure 含沒有改動機器及沒有回退證據的終態，不能統稱失敗；見 internal/deploy/deploy.go:106-110。
			return fmt.Sprintf("將建立新的 deployment，為 %d 台終態未成功的機器重新開單；不會沿用原本那張單的進度，也不會判斷機器停在哪一步。", eligibility.AffectedTargets)
		case "abandon":
			return fmt.Sprintf("將收成 finished；%d 台尚未開單的機器不會再開單。", eligibility.AffectedTargets)
		}
	}
	labels := make([]string, 0, len(eligibility.Blockers))
	for _, blocker := range eligibility.Blockers {
		labels = append(labels, DeploymentBlockerLabel(blocker))
	}
	return strings.Join(labels, "；")
}

type DeploymentActionEligibilitySet struct {
	Continue DeploymentActionEligibility `json:"continue"`
	Retry    DeploymentActionEligibility `json:"retry"`
	Abandon  DeploymentActionEligibility `json:"abandon"`
}

type DeploymentDetailResult struct {
	SchemaVersion int                            `json:"schema_version"`
	Consistency   string                         `json:"consistency"`
	EvaluatedAt   time.Time                      `json:"evaluated_at"`
	Item          DeploymentSummary              `json:"item"`
	Targets       []DeploymentTargetSummary      `json:"targets"`
	Independent   DeploymentIndependentSummary   `json:"independent"`
	Actions       DeploymentActionEligibilitySet `json:"actions"`
}

type DeploymentCreatePreviewRequest struct {
	Channel                 string `json:"channel"`
	Version                 string `json:"version"`
	ArtifactSHA256          string `json:"artifact_sha256"`
	BatchSize               int    `json:"batch_size"`
	ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
	Irreversible            bool   `json:"irreversible"`
}

type DeploymentArtifactPreview struct {
	Name                 string    `json:"name"`
	Version              string    `json:"version"`
	SHA256               string    `json:"sha256"`
	Digest               string    `json:"digest"`
	SizeBytes            int64     `json:"size_bytes"`
	EnginesNode          string    `json:"engines_node"`
	SHA512Integrity      string    `json:"sha512_integrity"`
	FetchedAt            time.Time `json:"fetched_at"`
	AvailableAndVerified bool      `json:"available_and_verified"`
}

type DeploymentPlanTargetPreview struct {
	MachineID      string  `json:"machine_id"`
	DisplayName    string  `json:"display_name"`
	Reachable      bool    `json:"reachable"`
	NodeVersion    *string `json:"node_version"`
	BatchNo        int     `json:"batch_no"`
	ExcludedReason *string `json:"excluded_reason"`
}

type DeploymentPromotionPreview struct {
	Allowed                  bool                                          `json:"allowed"`
	Blockers                 []string                                      `json:"blockers"`
	EarliestAt               *time.Time                                    `json:"earliest_at"`
	IndependentRequired      bool                                          `json:"independent_required"`
	IndependentPassedTargets int                                           `json:"independent_passed_targets"`
	IndependentTargets       []DeploymentPromotionIndependentTargetPreview `json:"independent_targets"`
}

type DeploymentPromotionIndependentTargetPreview struct {
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`
	JobID       string `json:"job_id"`
	State       string `json:"state"`
	NextStep    string `json:"next_step"`
}

const (
	PromotionNextStepNone                       = "none"
	PromotionNextStepAssignVerifier             = "assign_verifier"
	PromotionNextStepWaitForVerifier            = "wait_for_verifier"
	PromotionNextStepRerunVerifier              = "rerun_verifier"
	PromotionNextStepAssignActiveVerifier       = "assign_active_verifier"
	PromotionNextStepRerunCanary                = "rerun_canary"
	PromotionNextStepUpgradeAndReassignVerifier = "upgrade_and_reassign_verifier"
	PromotionNextStepReassignVerifier           = "reassign_verifier"
	PromotionNextStepRepairAndRerunCanary       = "repair_and_rerun_canary"
)

type DeploymentCreatePreviewResult struct {
	SchemaVersion           int                           `json:"schema_version"`
	PreviewedAt             time.Time                     `json:"previewed_at"`
	Channel                 string                        `json:"channel"`
	BatchSize               int                           `json:"batch_size"`
	ExecutionTimeoutSeconds int                           `json:"execution_timeout_seconds"`
	Irreversible            bool                          `json:"irreversible"`
	Artifact                DeploymentArtifactPreview     `json:"artifact"`
	Targets                 []DeploymentPlanTargetPreview `json:"targets"`
	Impact                  int                           `json:"impact"`
	Conflicts               int                           `json:"conflicts"`
	MissingPackages         int                           `json:"missing_packages"`
	UnknownNodes            int                           `json:"unknown_nodes"`
	Noncompliant            int                           `json:"noncompliant"`
	Unreachable             int                           `json:"unreachable"`
	TotalBatches            int                           `json:"total_batches"`
	Promotion               *DeploymentPromotionPreview   `json:"promotion"`
	CreateAllowed           bool                          `json:"create_allowed"`
	Blockers                []string                      `json:"blockers"`
	PreviewDigest           string                        `json:"preview_digest"`
}

func (s *Service) ListDeployments(request DeploymentListRequest, evaluatedAt time.Time) (DeploymentListResult, error) {
	if s == nil || s.store == nil || evaluatedAt.IsZero() {
		return DeploymentListResult{}, fmt.Errorf("%w: store and evaluated_at are required", ErrInvalidDeploymentRead)
	}
	normalized, err := normalizeDeploymentListRequest(request)
	if err != nil {
		return DeploymentListResult{}, err
	}
	filterDigest := deploymentFilterDigest(normalized)
	var after *deploymentListCursor
	if normalized.Cursor != "" {
		cursor, err := decodeDeploymentListCursor(normalized.Cursor, filterDigest)
		if err != nil {
			return DeploymentListResult{}, err
		}
		after = &cursor
	}
	views, err := s.store.ListDeployments(evaluatedAt.UTC())
	if err != nil {
		return DeploymentListResult{}, err
	}
	sort.SliceStable(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.After(views[j].CreatedAt)
		}
		return views[i].DeploymentID > views[j].DeploymentID
	})
	counts := make(map[string]int, len(deploymentStates))
	matching := make([]DeploymentSummary, 0, len(views))
	for _, view := range views {
		if !deploymentMatchesFilter(view, normalized) {
			continue
		}
		counts[view.State]++
		summary, err := projectDeploymentSummary(view)
		if err != nil {
			return DeploymentListResult{}, err
		}
		matching = append(matching, summary)
	}
	result := DeploymentListResult{
		SchemaVersion: DeploymentReadSchemaVersion, Consistency: DeploymentReadConsistencyLive,
		EvaluatedAt: evaluatedAt.UTC(), Total: len(matching),
		StateCounts: make([]DeploymentStateCount, 0, len(deploymentStates)), Items: make([]DeploymentSummary, 0, normalized.Limit),
	}
	for _, state := range deploymentStates {
		result.StateCounts = append(result.StateCounts, DeploymentStateCount{State: state, Count: counts[state]})
	}
	start := 0
	if after != nil {
		start = len(matching)
		for i, item := range matching {
			if deploymentPositionAfter(item.CreatedAt, item.DeploymentID, after.CreatedAt, after.DeploymentID) {
				start = i
				break
			}
		}
	}
	end := start + normalized.Limit
	if end > len(matching) {
		end = len(matching)
	}
	result.Items = append(result.Items, matching[start:end]...)
	if end < len(matching) && len(result.Items) > 0 {
		last := result.Items[len(result.Items)-1]
		encoded, err := encodeDeploymentListCursor(deploymentListCursor{
			Version: DeploymentReadSchemaVersion, FilterDigest: filterDigest,
			CreatedAt: last.CreatedAt, DeploymentID: last.DeploymentID,
		})
		if err != nil {
			return DeploymentListResult{}, err
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func (s *Service) DeploymentDetail(deploymentID string, evaluatedAt time.Time) (DeploymentDetailResult, error) {
	if s == nil || s.store == nil || evaluatedAt.IsZero() || !validDeploymentIdentifier(deploymentID, 256) {
		return DeploymentDetailResult{}, fmt.Errorf("%w: deployment_id and evaluated_at are required", ErrInvalidDeploymentRead)
	}
	evaluatedAt = evaluatedAt.UTC()
	view, err := s.store.DeploymentView(deploymentID, evaluatedAt)
	if err != nil {
		return DeploymentDetailResult{}, err
	}
	summary, err := projectDeploymentSummary(view)
	if err != nil {
		return DeploymentDetailResult{}, err
	}
	targets, err := projectDeploymentTargets(view)
	if err != nil {
		return DeploymentDetailResult{}, err
	}
	actions, err := s.deploymentActionEligibility(view, summary.Material, evaluatedAt)
	if err != nil {
		return DeploymentDetailResult{}, err
	}
	verdicts, err := s.store.DeploymentIndependentVerdicts(deploymentID)
	if err != nil {
		return DeploymentDetailResult{}, err
	}
	independent := attachDeploymentIndependentVerdicts(targets, verdicts)
	return DeploymentDetailResult{
		SchemaVersion: DeploymentReadSchemaVersion, Consistency: DeploymentReadConsistencyLive, EvaluatedAt: evaluatedAt,
		Item: summary, Targets: targets, Independent: independent, Actions: actions,
	}, nil
}

// attachDeploymentIndependentVerdicts gives every opened target a verdict and
// counts them. A target whose job has no independent row gets absent rather
// than nil: the job exists, the ledger was read, and nothing was found. Only a
// target with no job at all is left nil.
func attachDeploymentIndependentVerdicts(targets []DeploymentTargetSummary,
	verdicts map[string]store.DeploymentIndependentVerdict,
) DeploymentIndependentSummary {
	counts := make(map[store.IndependentVerdict]int, len(deploymentIndependentVerdictOrder))
	summary := DeploymentIndependentSummary{
		Verdicts: make([]DeploymentIndependentVerdictCount, 0, len(deploymentIndependentVerdictOrder)),
	}
	for i := range targets {
		target := &targets[i]
		if target.JobID == nil {
			continue
		}
		verdict := DeploymentTargetIndependent{Verdict: string(store.IndependentAbsent)}
		if found, ok := verdicts[*target.JobID]; ok {
			verdict = DeploymentTargetIndependent{
				Verdict: string(found.Verdict), Rows: found.Rows, LiveProducers: found.LiveProducers,
			}
		}
		target.Independent = &verdict
		summary.OpenedTargets++
		summary.LiveProducers += verdict.LiveProducers
		counts[store.IndependentVerdict(verdict.Verdict)]++
	}
	for _, verdict := range deploymentIndependentVerdictOrder {
		summary.Verdicts = append(summary.Verdicts,
			DeploymentIndependentVerdictCount{Verdict: string(verdict), Targets: counts[verdict]})
	}
	summary.PassedTargets = counts[store.IndependentPassed]
	return summary
}

func (s *Service) PreviewDeploymentCreate(request DeploymentCreatePreviewRequest, evaluatedAt time.Time) (DeploymentCreatePreviewResult, error) {
	return s.PreviewDeploymentCreateContext(context.Background(), request, evaluatedAt)
}

// PreviewDeploymentCreateContext keeps the potentially large artifact proof
// bound to the caller lifecycle. Non-request callers may use
// PreviewDeploymentCreate, which retains the historical background context.
func (s *Service) PreviewDeploymentCreateContext(ctx context.Context, request DeploymentCreatePreviewRequest,
	evaluatedAt time.Time,
) (DeploymentCreatePreviewResult, error) {
	if ctx == nil {
		return DeploymentCreatePreviewResult{}, fmt.Errorf("%w: context is required", ErrInvalidDeploymentPreview)
	}
	if err := ctx.Err(); err != nil {
		return DeploymentCreatePreviewResult{}, err
	}
	if s == nil || s.store == nil || evaluatedAt.IsZero() {
		return DeploymentCreatePreviewResult{}, fmt.Errorf("%w: store and evaluated_at are required", ErrInvalidDeploymentPreview)
	}
	normalized, err := normalizeDeploymentCreatePreviewRequest(request)
	if err != nil {
		return DeploymentCreatePreviewResult{}, err
	}
	if strings.TrimSpace(s.artifactsDir) == "" {
		return DeploymentCreatePreviewResult{}, fmt.Errorf("%w: Hub artifact catalog is not configured", ErrInvalidDeploymentPreview)
	}
	material, err := artifact.ResolveOpenClawMaterialContext(ctx, s.artifactsDir, normalized.Version, normalized.ArtifactSHA256)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return DeploymentCreatePreviewResult{}, ctxErr
		}
		return DeploymentCreatePreviewResult{}, fmt.Errorf("%w: %v", ErrInvalidDeploymentPreview, err)
	}
	if err := validateDeploymentArtifactPreviewMaterial(material); err != nil {
		return DeploymentCreatePreviewResult{}, err
	}
	plan, _, err := s.store.PlanChannelDeployment(normalized.Channel, material.EnginesNode, normalized.BatchSize, evaluatedAt.UTC())
	if err != nil {
		return DeploymentCreatePreviewResult{}, err
	}
	targets, err := projectDeploymentPlan(plan, normalized.BatchSize)
	if err != nil {
		return DeploymentCreatePreviewResult{}, err
	}
	result := DeploymentCreatePreviewResult{
		SchemaVersion: DeploymentPreviewSchemaVersion, PreviewedAt: evaluatedAt.UTC(),
		Channel: normalized.Channel, BatchSize: normalized.BatchSize,
		ExecutionTimeoutSeconds: normalized.ExecutionTimeoutSeconds, Irreversible: normalized.Irreversible,
		Artifact: deploymentArtifactPreview(material),
		Targets:  targets,
		Impact:   plan.Impact, Conflicts: plan.Conflicts, MissingPackages: plan.MissingPackages,
		UnknownNodes: plan.UnknownNodes, Noncompliant: plan.Noncompliant,
		Unreachable: plan.Unreachable, TotalBatches: plan.TotalBatches,
		CreateAllowed: plan.Impact > 0, Blockers: []string{},
	}
	if plan.Impact == 0 {
		result.Blockers = append(result.Blockers, "no_included_targets")
	}
	if normalized.Channel == "stable" {
		decision, err := s.store.PreviewStableOpenClawPromotion(material.Version, material.Artifact.SHA256, evaluatedAt.UTC())
		if err != nil {
			return DeploymentCreatePreviewResult{}, err
		}
		promotion := projectDeploymentPromotion(decision)
		result.Promotion = &promotion
		if !decision.Allowed {
			result.CreateAllowed = false
			result.Blockers = append(result.Blockers, "stable_promotion_locked")
		}
	}
	result.PreviewDigest = deploymentCreatePreviewDigest(result)
	return result, nil
}

func normalizeDeploymentListRequest(request DeploymentListRequest) (DeploymentListRequest, error) {
	if request.Channel != "" && request.Channel != "canary" && request.Channel != "stable" {
		return DeploymentListRequest{}, fmt.Errorf("%w: channel is invalid", ErrInvalidDeploymentRead)
	}
	if request.Limit == 0 {
		request.Limit = DefaultDeploymentReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxDeploymentReadLimit {
		return DeploymentListRequest{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidDeploymentRead, MaxDeploymentReadLimit)
	}
	if len(request.States) > len(deploymentStates) {
		return DeploymentListRequest{}, fmt.Errorf("%w: too many states", ErrInvalidDeploymentRead)
	}
	request.States = append([]string(nil), request.States...)
	seen := make(map[string]bool, len(request.States))
	for _, state := range request.States {
		if !isDeploymentState(state) || seen[state] {
			return DeploymentListRequest{}, fmt.Errorf("%w: state filter is invalid", ErrInvalidDeploymentRead)
		}
		seen[state] = true
	}
	sort.Slice(request.States, func(i, j int) bool {
		return deploymentStateOrder(request.States[i]) < deploymentStateOrder(request.States[j])
	})
	if request.Cursor != "" && (strings.TrimSpace(request.Cursor) != request.Cursor || len(request.Cursor) > 2048) {
		return DeploymentListRequest{}, fmt.Errorf("%w: cursor is invalid", ErrInvalidDeploymentRead)
	}
	return request, nil
}

func normalizeDeploymentCreatePreviewRequest(request DeploymentCreatePreviewRequest) (DeploymentCreatePreviewRequest, error) {
	if request.Channel != strings.TrimSpace(request.Channel) || request.Version != strings.TrimSpace(request.Version) ||
		request.ArtifactSHA256 != strings.TrimSpace(request.ArtifactSHA256) {
		return DeploymentCreatePreviewRequest{}, fmt.Errorf("%w: inputs must not have surrounding whitespace", ErrInvalidDeploymentPreview)
	}
	if request.BatchSize == 0 {
		request.BatchSize = DefaultDeploymentBatchSize
	}
	if request.ExecutionTimeoutSeconds == 0 {
		request.ExecutionTimeoutSeconds = DefaultDeploymentTimeout
	}
	if request.Channel != "canary" && request.Channel != "stable" {
		return DeploymentCreatePreviewRequest{}, fmt.Errorf("%w: channel must be canary or stable", ErrInvalidDeploymentPreview)
	}
	if !validDeploymentIdentifier(request.Version, 128) {
		return DeploymentCreatePreviewRequest{}, fmt.Errorf("%w: version is invalid", ErrInvalidDeploymentPreview)
	}
	if request.ArtifactSHA256 != "" && !artifact.ValidSHA256Hex(request.ArtifactSHA256) {
		return DeploymentCreatePreviewRequest{}, fmt.Errorf("%w: artifact_sha256 is not canonical", ErrInvalidDeploymentPreview)
	}
	if request.BatchSize < 1 || request.BatchSize > store.MaxDeploymentBatchSize {
		return DeploymentCreatePreviewRequest{}, fmt.Errorf("%w: batch_size must be between 1 and %d", ErrInvalidDeploymentPreview, store.MaxDeploymentBatchSize)
	}
	if request.ExecutionTimeoutSeconds < 1 || request.ExecutionTimeoutSeconds > 86400 {
		return DeploymentCreatePreviewRequest{}, fmt.Errorf("%w: execution_timeout_seconds must be between 1 and 86400", ErrInvalidDeploymentPreview)
	}
	return request, nil
}

func deploymentMatchesFilter(view store.DeploymentView, request DeploymentListRequest) bool {
	if request.Channel != "" && view.Channel != request.Channel {
		return false
	}
	if len(request.States) > 0 {
		matched := false
		for _, state := range request.States {
			matched = matched || view.State == state
		}
		if !matched {
			return false
		}
	}
	return request.Stuck == nil || *request.Stuck == (view.Stuck > 0)
}

func projectDeploymentSummary(view store.DeploymentView) (DeploymentSummary, error) {
	d := view.Deployment
	if !validDeploymentIdentifier(d.DeploymentID, 256) || !validDeploymentIdentifier(d.DesiredID, 256) ||
		!validDeploymentIdentifier(d.ResourceKind, 128) || !validDeploymentIdentifier(d.ResourceID, 256) ||
		(d.Channel != "canary" && d.Channel != "stable") || !isDeploymentState(d.State) ||
		d.Revision <= 0 || d.ControlRevision < 0 || d.BatchSize < 1 || d.BatchSize > store.MaxDeploymentBatchSize ||
		d.CreatedAt.IsZero() || view.OpenedBatch < 1 || view.TotalBatches < 1 || view.OpenedBatch > view.TotalBatches ||
		view.Attempt < 1 || view.Stuck < 0 || view.TerminalStuck < 0 || view.SilentStuck < 0 ||
		view.Stuck != view.TerminalStuck+view.SilentStuck {
		return DeploymentSummary{}, fmt.Errorf("%w: stored deployment is incoherent", ErrInvalidDeploymentRead)
	}
	if d.RetryOf != "" && !validDeploymentIdentifier(d.RetryOf, 256) {
		return DeploymentSummary{}, fmt.Errorf("%w: retry lineage identity is invalid", ErrInvalidDeploymentRead)
	}
	if d.State == store.DeploymentPaused && d.PausedAt == nil {
		return DeploymentSummary{}, fmt.Errorf("%w: paused deployment lacks paused_at", ErrInvalidDeploymentRead)
	}
	if d.State == store.DeploymentFinished && d.FinishedAt == nil {
		return DeploymentSummary{}, fmt.Errorf("%w: finished deployment lacks finished_at", ErrInvalidDeploymentRead)
	}
	if d.State != store.DeploymentFinished && d.FinishedAt != nil {
		return DeploymentSummary{}, fmt.Errorf("%w: non-finished deployment has finished_at", ErrInvalidDeploymentRead)
	}
	// List and detail both fail closed on a malformed target snapshot. Otherwise
	// aggregate counts could look trustworthy while their underlying plan is not.
	if _, err := projectDeploymentTargets(view); err != nil {
		return DeploymentSummary{}, err
	}
	material := projectDeploymentMaterial(d.ResourceKind, d.ResourceID, d.Spec)
	result := DeploymentSummary{
		DeploymentID: d.DeploymentID, Channel: d.Channel, DesiredID: d.DesiredID,
		ResourceKind: d.ResourceKind, ResourceID: d.ResourceID,
		DesiredRevision: d.Revision, ControlRevision: d.ControlRevision,
		BatchSize: d.BatchSize, State: d.State, CreatedAt: d.CreatedAt.UTC(),
		PausedAt: utcTimePtr(d.PausedAt), FinishedAt: utcTimePtr(d.FinishedAt),
		Attempt: view.Attempt, OpenedBatch: view.OpenedBatch, TotalBatches: view.TotalBatches,
		Stuck: view.Stuck, TerminalStuck: view.TerminalStuck, SilentStuck: view.SilentStuck,
		JobStateCounts: make([]JobStateCount, 0, len(deploy.AllJobStates)), Material: material,
	}
	if d.RetryOf != "" {
		value := d.RetryOf
		result.RetryOf = &value
	}
	for _, state := range deploy.AllJobStates {
		result.JobStateCounts = append(result.JobStateCounts, JobStateCount{State: state, Count: view.Counts[state]})
	}
	if view.BoundaryPause != nil {
		pause := view.BoundaryPause
		if pause.OpenedBatch < 1 || pause.OpenedBatch > view.OpenedBatch ||
			!isDeploymentPauseKind(pause.Kind) || !validDeploymentText(pause.Reason, 4096, false) ||
			pause.PausedAt.IsZero() {
			return DeploymentSummary{}, fmt.Errorf("%w: boundary pause is incoherent", ErrInvalidDeploymentRead)
		}
		result.BoundaryPause = &DeploymentBoundarySummary{
			OpenedBatch: pause.OpenedBatch, Kind: pause.Kind, PausedAt: pause.PausedAt.UTC(),
		}
	}
	return result, nil
}

func projectDeploymentMaterial(resourceKind, resourceID, raw string) DeploymentMaterialSummary {
	result := DeploymentMaterialSummary{Status: DeploymentMaterialUnsupported, Kind: resourceKind}
	if resourceKind != "openclaw" {
		return result
	}
	result.Status = DeploymentMaterialInvalid
	if resourceID != "openclaw" {
		return result
	}
	var spec model.OpenClawSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || spec.Kind != "openclaw" ||
		!validDeploymentIdentifier(spec.Version, 128) || spec.Artifact == nil ||
		!artifact.ValidSHA256Hex(spec.Artifact.SHA256) || spec.Artifact.Size < 0 ||
		!validDeploymentText(spec.Artifact.EnginesNode, 512, true) {
		return result
	}
	version, digest, size, engines := spec.Version, "sha256:"+spec.Artifact.SHA256, spec.Artifact.Size, spec.Artifact.EnginesNode
	result.Status, result.Kind = DeploymentMaterialRecorded, spec.Kind
	result.Version, result.ArtifactDigest, result.SizeBytes = &version, &digest, &size
	if engines != "" {
		result.EnginesNode = &engines
	}
	return result
}

func projectDeploymentTargets(view store.DeploymentView) ([]DeploymentTargetSummary, error) {
	result := make([]DeploymentTargetSummary, 0, len(view.Targets))
	counts := make(map[deploy.JobState]int)
	batchCounts := make(map[int]int)
	seenMachines := make(map[string]bool, len(view.Targets))
	seenJobs := make(map[string]bool, len(view.Targets))
	opened, total, terminalStuck, silentStuck := 0, 0, 0, 0
	for _, target := range view.Targets {
		if !validDeploymentIdentifier(target.MachineID, 256) ||
			!validDeploymentText(target.DisplayName, 256, false) || target.BatchNo < 0 || seenMachines[target.MachineID] {
			return nil, fmt.Errorf("%w: stored target identity is invalid", ErrInvalidDeploymentRead)
		}
		seenMachines[target.MachineID] = true
		item := DeploymentTargetSummary{MachineID: target.MachineID, DisplayName: target.DisplayName, BatchNo: target.BatchNo}
		if target.ExcludedReason != "" {
			if target.BatchNo != 0 || target.JobID != "" || !isDeploymentExclusion(target.ExcludedReason) {
				return nil, fmt.Errorf("%w: excluded target has a job or batch", ErrInvalidDeploymentRead)
			}
			value := target.ExcludedReason
			item.ExcludedReason = &value
		} else {
			if target.BatchNo < 1 {
				return nil, fmt.Errorf("%w: included target has no batch", ErrInvalidDeploymentRead)
			}
			batchCounts[target.BatchNo]++
			if batchCounts[target.BatchNo] > view.BatchSize || batchCounts[target.BatchNo] > store.MaxDeploymentBatchSize {
				return nil, fmt.Errorf("%w: stored batch exceeds batch_size", ErrInvalidDeploymentRead)
			}
			if target.BatchNo > total {
				total = target.BatchNo
			}
			if target.JobID != "" {
				if !validDeploymentIdentifier(target.JobID, 256) || seenJobs[target.JobID] || target.JobReferences != 1 ||
					target.JobMachineID != target.MachineID || target.JobDesiredID != view.DesiredID ||
					target.JobRevision != view.Revision {
					return nil, fmt.Errorf("%w: opened target job link is invalid", ErrInvalidDeploymentRead)
				}
				seenJobs[target.JobID] = true
				if !deploy.IsKnownJobState(target.JobState) ||
					target.CreatedAt.IsZero() || target.LastActivity.IsZero() ||
					(deploy.IsTerminal(target.JobState) != (target.TerminalAt != nil)) {
					return nil, fmt.Errorf("%w: opened target job is invalid", ErrInvalidDeploymentRead)
				}
				if target.BatchNo > opened {
					opened = target.BatchNo
				}
				jobID, state, created, activity := target.JobID, target.JobState, target.CreatedAt.UTC(), target.LastActivity.UTC()
				item.JobID, item.JobState, item.JobCreatedAt = &jobID, &state, &created
				if !target.LastActivity.IsZero() {
					item.LastActivityAt = &activity
				}
				item.TerminalAt = utcTimePtr(target.TerminalAt)
				counts[target.JobState]++
			} else if target.JobMachineID != "" || target.JobDesiredID != "" || target.JobRevision != 0 ||
				target.JobReferences != 0 || target.JobState != "" || !target.CreatedAt.IsZero() ||
				target.TerminalAt != nil || !target.LastActivity.IsZero() {
				return nil, fmt.Errorf("%w: unopened target contains job state", ErrInvalidDeploymentRead)
			}
		}
		if target.StuckKind != "" {
			if target.JobID == "" ||
				(target.StuckKind == "terminal_failure" && (!deploy.IsTerminal(target.JobState) || target.JobState == deploy.Succeeded)) ||
				(target.StuckKind == "no_event" && deploy.IsTerminal(target.JobState)) ||
				(target.StuckKind != "terminal_failure" && target.StuckKind != "no_event") {
				return nil, fmt.Errorf("%w: target stuck classification is invalid", ErrInvalidDeploymentRead)
			}
			kind := target.StuckKind
			item.StuckKind = &kind
			if kind == "terminal_failure" {
				terminalStuck++
			} else {
				silentStuck++
			}
		}
		if target.JobID != "" && deploy.IsTerminal(target.JobState) && target.JobState != deploy.Succeeded &&
			target.StuckKind != "terminal_failure" {
			return nil, fmt.Errorf("%w: terminal failure lacks stuck classification", ErrInvalidDeploymentRead)
		}
		result = append(result, item)
	}
	if total == 0 || opened == 0 || len(batchCounts) != total {
		return nil, fmt.Errorf("%w: stored batches are not contiguous", ErrInvalidDeploymentRead)
	}
	for batchNo := 1; batchNo <= opened; batchNo++ {
		for _, target := range view.Targets {
			if target.ExcludedReason == "" && target.BatchNo == batchNo && target.JobID == "" {
				return nil, fmt.Errorf("%w: opened batch prefix contains a gap", ErrInvalidDeploymentRead)
			}
		}
	}
	if total != view.TotalBatches || opened != view.OpenedBatch || terminalStuck != view.TerminalStuck ||
		silentStuck != view.SilentStuck || terminalStuck+silentStuck != view.Stuck {
		return nil, fmt.Errorf("%w: target aggregates do not match deployment", ErrInvalidDeploymentRead)
	}
	for state, count := range view.Counts {
		if !deploy.IsKnownJobState(state) || count < 0 || counts[state] != count {
			return nil, fmt.Errorf("%w: job state counts do not match targets", ErrInvalidDeploymentRead)
		}
	}
	for _, state := range deploy.AllJobStates {
		if view.Counts[state] != counts[state] {
			return nil, fmt.Errorf("%w: job state counts do not match targets", ErrInvalidDeploymentRead)
		}
	}
	if view.DesiredJobCount != len(seenJobs) {
		return nil, fmt.Errorf("%w: deployment desired-state jobs do not match target links", ErrInvalidDeploymentRead)
	}
	return result, nil
}

func (s *Service) deploymentActionEligibility(view store.DeploymentView, material DeploymentMaterialSummary, now time.Time) (DeploymentActionEligibilitySet, error) {
	set := DeploymentActionEligibilitySet{
		Continue: DeploymentActionEligibility{Outcome: "open_next_batch", Blockers: []string{}},
		Retry:    DeploymentActionEligibility{Outcome: "create_retry_attempt", Blockers: []string{}},
		Abandon:  DeploymentActionEligibility{Outcome: "finish_without_unopened_batches", Blockers: []string{}},
	}
	nonterminal, retryTargets, unopened := 0, 0, 0
	for _, target := range view.Targets {
		if target.JobID != "" && !deploy.IsTerminal(target.JobState) {
			nonterminal++
		}
		if target.StuckKind == "terminal_failure" {
			retryTargets++
		}
		if target.ExcludedReason == "" && target.JobID == "" {
			unopened++
		}
	}
	set.Continue.AffectedTargets = 0
	if view.OpenedBatch < view.TotalBatches {
		for _, target := range view.Targets {
			if target.BatchNo == view.OpenedBatch+1 && target.JobID == "" && target.ExcludedReason == "" {
				set.Continue.AffectedTargets++
			}
		}
	} else {
		set.Continue.Outcome = "finish"
	}
	set.Retry.AffectedTargets = retryTargets
	set.Abandon.AffectedTargets = unopened
	if view.State != store.DeploymentPaused {
		set.Continue.Blockers = append(set.Continue.Blockers, "deployment_not_paused")
		set.Abandon.Blockers = append(set.Abandon.Blockers, "deployment_not_paused")
	}
	if view.State != store.DeploymentPaused && view.State != store.DeploymentFinished {
		set.Retry.Blockers = append(set.Retry.Blockers, "deployment_not_retryable")
	}
	if nonterminal > 0 {
		set.Continue.Blockers = append(set.Continue.Blockers, "nonterminal_jobs")
		set.Retry.Blockers = append(set.Retry.Blockers, "nonterminal_jobs")
		set.Abandon.Blockers = append(set.Abandon.Blockers, "nonterminal_jobs")
	}
	if retryTargets == 0 {
		set.Retry.Blockers = append(set.Retry.Blockers, "no_terminal_failure_targets")
	}
	if view.OpenedBatch == 0 {
		set.Continue.Blockers = append(set.Continue.Blockers, "no_opened_batch")
		set.Retry.Blockers = append(set.Retry.Blockers, "no_opened_batch")
	}
	if view.OpenedBatch < view.TotalBatches && set.Continue.AffectedTargets == 0 {
		set.Continue.Blockers = append(set.Continue.Blockers, "next_batch_empty")
	}
	if material.Status != DeploymentMaterialRecorded {
		if view.OpenedBatch < view.TotalBatches {
			set.Continue.Blockers = append(set.Continue.Blockers, "invalid_material")
		}
		set.Retry.Blockers = append(set.Retry.Blockers, "invalid_material")
	}
	if view.Channel == "stable" && material.Status == DeploymentMaterialRecorded && material.Version != nil && material.ArtifactDigest != nil {
		decision, err := s.store.PreviewStableOpenClawPromotion(*material.Version, strings.TrimPrefix(*material.ArtifactDigest, "sha256:"), now)
		if err != nil {
			return DeploymentActionEligibilitySet{}, err
		}
		if !decision.Allowed {
			if view.OpenedBatch < view.TotalBatches {
				set.Continue.Blockers = append(set.Continue.Blockers, "stable_promotion_locked")
			}
			set.Retry.Blockers = append(set.Retry.Blockers, "stable_promotion_locked")
		}
	}
	views, err := s.store.ListDeployments(now)
	if err != nil {
		return DeploymentActionEligibilitySet{}, err
	}
	for _, candidate := range views {
		if candidate.DeploymentID != view.DeploymentID &&
			(candidate.State == store.DeploymentRunning || candidate.State == store.DeploymentPaused) &&
			candidate.ResourceKind == view.ResourceKind && candidate.ResourceID == view.ResourceID {
			set.Continue.Blockers = append(set.Continue.Blockers, "active_resource_deployment")
			set.Retry.Blockers = append(set.Retry.Blockers, "active_resource_deployment")
			set.Abandon.Blockers = append(set.Abandon.Blockers, "active_resource_deployment")
			break
		}
	}
	if view.ControlRevision == store.MaxDeploymentControlRevision {
		set.Continue.Blockers = append(set.Continue.Blockers, "control_revision_exhausted")
		set.Retry.Blockers = append(set.Retry.Blockers, "control_revision_exhausted")
		set.Abandon.Blockers = append(set.Abandon.Blockers, "control_revision_exhausted")
	}
	set.Continue.Eligible = len(set.Continue.Blockers) == 0
	set.Retry.Eligible = len(set.Retry.Blockers) == 0
	set.Abandon.Eligible = len(set.Abandon.Blockers) == 0
	return set, nil
}

func validateDeploymentArtifactPreviewMaterial(material artifact.OpenClawMaterial) error {
	record := material.Artifact
	parsedURL, urlErr := url.Parse(record.TarballURL)
	if record.Name != "openclaw" || material.Version != record.Version || material.EnginesNode != record.EnginesNode ||
		material.Digest != "sha256:"+record.SHA256 || !validDeploymentIdentifier(record.Version, 128) ||
		!artifact.ValidSHA256Hex(record.SHA256) || record.Size < 0 || record.FetchedAt.IsZero() ||
		!validDeploymentText(record.EnginesNode, 512, true) ||
		!validDeploymentText(record.TarballURL, 2048, false) ||
		!validDeploymentText(record.SHA512Integrity, 1024, false) ||
		!validDeploymentText(record.FetchedBy, 256, true) || urlErr != nil || parsedURL.Host == "" ||
		(parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.User != nil {
		return fmt.Errorf("%w: selected artifact metadata is invalid", ErrInvalidDeploymentPreview)
	}
	return nil
}

func projectDeploymentPlan(plan rollout.DeploymentPlan, batchSize int) ([]DeploymentPlanTargetPreview, error) {
	if batchSize < 1 || batchSize > store.MaxDeploymentBatchSize || plan.Impact < 0 || plan.Conflicts < 0 ||
		plan.MissingPackages < 0 || plan.UnknownNodes < 0 || plan.Unreachable < 0 || plan.TotalBatches < 0 {
		return nil, fmt.Errorf("%w: deployment plan aggregates are invalid", ErrInvalidDeploymentPreview)
	}
	result := make([]DeploymentPlanTargetPreview, 0, len(plan.Targets))
	seen := make(map[string]bool, len(plan.Targets))
	batchCounts := make(map[int]int)
	impact, conflicts, missing, unknown, noncompliant, unreachable, maxBatch := 0, 0, 0, 0, 0, 0, 0
	previousName, previousID := "", ""
	for i, target := range plan.Targets {
		if !validDeploymentIdentifier(target.MachineID, 256) ||
			!validDeploymentText(target.DisplayName, 256, false) ||
			!validDeploymentText(target.NodeVersion, 128, true) || seen[target.MachineID] || target.BatchNo < 0 {
			return nil, fmt.Errorf("%w: deployment plan target is invalid", ErrInvalidDeploymentPreview)
		}
		if i > 0 && (target.DisplayName < previousName ||
			(target.DisplayName == previousName && target.MachineID <= previousID)) {
			return nil, fmt.Errorf("%w: deployment target plan is not canonically ordered", ErrInvalidDeploymentPreview)
		}
		previousName, previousID = target.DisplayName, target.MachineID
		seen[target.MachineID] = true
		item := DeploymentPlanTargetPreview{
			MachineID: target.MachineID, DisplayName: target.DisplayName,
			Reachable: target.Reachable, BatchNo: target.BatchNo,
		}
		if target.NodeVersion != "" {
			value := target.NodeVersion
			item.NodeVersion = &value
		}
		if target.ExcludedReason != "" {
			if !isDeploymentExclusion(target.ExcludedReason) || target.BatchNo != 0 {
				return nil, fmt.Errorf("%w: deployment exclusion is invalid", ErrInvalidDeploymentPreview)
			}
			value := target.ExcludedReason
			item.ExcludedReason = &value
			switch target.ExcludedReason {
			case rollout.ExcludedConflict:
				conflicts++
			case rollout.ExcludedMissingPackage:
				missing++
			case rollout.ExcludedUnknownNode:
				unknown++
			case rollout.ExcludedNoncompliant:
				noncompliant++
			}
		} else {
			if target.BatchNo < 1 {
				return nil, fmt.Errorf("%w: included deployment target has no batch", ErrInvalidDeploymentPreview)
			}
			impact++
			batchCounts[target.BatchNo]++
			if batchCounts[target.BatchNo] > batchSize {
				return nil, fmt.Errorf("%w: deployment plan exceeds batch_size", ErrInvalidDeploymentPreview)
			}
			if target.BatchNo > maxBatch {
				maxBatch = target.BatchNo
			}
			if !target.Reachable {
				unreachable++
			}
		}
		result = append(result, item)
	}
	if (maxBatch > 0 && len(batchCounts) != maxBatch) || impact != plan.Impact || conflicts != plan.Conflicts ||
		missing != plan.MissingPackages || unknown != plan.UnknownNodes || unreachable != plan.Unreachable ||
		noncompliant != plan.Noncompliant || maxBatch != plan.TotalBatches ||
		impact+conflicts+missing+unknown+noncompliant != len(plan.Targets) {
		return nil, fmt.Errorf("%w: deployment plan aggregates do not match targets", ErrInvalidDeploymentPreview)
	}
	return result, nil
}

func deploymentArtifactPreview(material artifact.OpenClawMaterial) DeploymentArtifactPreview {
	record := material.Artifact
	return DeploymentArtifactPreview{
		Name: record.Name, Version: record.Version, SHA256: record.SHA256, Digest: material.Digest,
		SizeBytes: record.Size, EnginesNode: record.EnginesNode,
		SHA512Integrity: record.SHA512Integrity, FetchedAt: record.FetchedAt.UTC(), AvailableAndVerified: true,
	}
}

func projectDeploymentPromotion(decision rollout.PromoteDecision) DeploymentPromotionPreview {
	result := DeploymentPromotionPreview{
		Allowed: decision.Allowed, Blockers: []string{}, IndependentRequired: true,
		IndependentTargets: []DeploymentPromotionIndependentTargetPreview{},
	}
	if !decision.Allowed {
		// Store reasons are presentation strings and may contain display names or
		// host errors. The public contract exposes a bounded semantic code only.
		result.Blockers = append(result.Blockers, "stable_promotion_locked")
	}
	if !decision.EarliestAt.IsZero() {
		value := decision.EarliestAt.UTC()
		result.EarliestAt = &value
	}
	for _, target := range decision.IndependentTargets {
		state := string(target.State)
		if target.State == rollout.IndependentGatePassed {
			result.IndependentPassedTargets++
		}
		result.IndependentTargets = append(result.IndependentTargets,
			DeploymentPromotionIndependentTargetPreview{
				MachineID: target.MachineID, DisplayName: target.DisplayName,
				JobID: target.JobID, State: state, NextStep: promotionIndependentNextStep(target.State),
			})
	}
	return result
}

func promotionIndependentNextStep(state rollout.IndependentGateState) string {
	switch state {
	case rollout.IndependentGatePassed:
		return PromotionNextStepNone
	case rollout.IndependentGateAwaitingReport:
		return PromotionNextStepWaitForVerifier
	case rollout.IndependentGateIncompleteReport:
		return PromotionNextStepRerunVerifier
	case rollout.IndependentGateProducerRevoked:
		return PromotionNextStepAssignActiveVerifier
	case rollout.IndependentGateDigestMismatch:
		return PromotionNextStepRerunCanary
	case rollout.IndependentGateReleaseMismatch:
		return PromotionNextStepRepairAndRerunCanary
	case rollout.IndependentGateReleaseUnreported:
		return PromotionNextStepUpgradeAndReassignVerifier
	case rollout.IndependentGateStale:
		return PromotionNextStepReassignVerifier
	case rollout.IndependentGateFailed:
		return PromotionNextStepRepairAndRerunCanary
	default:
		return PromotionNextStepAssignVerifier
	}
}

func deploymentCreatePreviewDigest(result DeploymentCreatePreviewResult) string {
	type artifactIdentity struct {
		Name          string `json:"name"`
		Version       string `json:"version"`
		SHA256        string `json:"sha256"`
		SizeBytes     int64  `json:"size_bytes"`
		EnginesNode   string `json:"engines_node"`
		SHA512        string `json:"sha512_integrity"`
		BytesVerified bool   `json:"bytes_verified"`
	}
	body := struct {
		Version                 int                           `json:"version"`
		Channel                 string                        `json:"channel"`
		BatchSize               int                           `json:"batch_size"`
		ExecutionTimeoutSeconds int                           `json:"execution_timeout_seconds"`
		Irreversible            bool                          `json:"irreversible"`
		Artifact                artifactIdentity              `json:"artifact"`
		Targets                 []DeploymentPlanTargetPreview `json:"targets"`
		Impact                  int                           `json:"impact"`
		Conflicts               int                           `json:"conflicts"`
		MissingPackages         int                           `json:"missing_packages"`
		UnknownNodes            int                           `json:"unknown_nodes"`
		Noncompliant            int                           `json:"noncompliant"`
		Unreachable             int                           `json:"unreachable"`
		TotalBatches            int                           `json:"total_batches"`
		Promotion               *DeploymentPromotionPreview   `json:"promotion"`
		CreateAllowed           bool                          `json:"create_allowed"`
		Blockers                []string                      `json:"blockers"`
	}{
		Version: DeploymentPreviewSchemaVersion, Channel: result.Channel, BatchSize: result.BatchSize,
		ExecutionTimeoutSeconds: result.ExecutionTimeoutSeconds, Irreversible: result.Irreversible,
		Artifact: artifactIdentity{
			Name: result.Artifact.Name, Version: result.Artifact.Version,
			SHA256: result.Artifact.SHA256, SizeBytes: result.Artifact.SizeBytes,
			EnginesNode: result.Artifact.EnginesNode, SHA512: result.Artifact.SHA512Integrity,
			BytesVerified: result.Artifact.AvailableAndVerified,
		}, Targets: result.Targets,
		Impact: result.Impact, Conflicts: result.Conflicts, MissingPackages: result.MissingPackages,
		UnknownNodes: result.UnknownNodes, Noncompliant: result.Noncompliant,
		Unreachable: result.Unreachable, TotalBatches: result.TotalBatches,
		Promotion: result.Promotion, CreateAllowed: result.CreateAllowed, Blockers: result.Blockers,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type deploymentListCursor struct {
	Version      int       `json:"v"`
	FilterDigest string    `json:"filter_digest"`
	CreatedAt    time.Time `json:"created_at"`
	DeploymentID string    `json:"deployment_id"`
}

func deploymentFilterDigest(request DeploymentListRequest) string {
	stuck := "any"
	if request.Stuck != nil {
		stuck = fmt.Sprintf("%t", *request.Stuck)
	}
	body := struct {
		Version int      `json:"v"`
		Channel string   `json:"channel"`
		States  []string `json:"states"`
		Stuck   string   `json:"stuck"`
	}{DeploymentReadSchemaVersion, request.Channel, request.States, stuck}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodeDeploymentListCursor(cursor deploymentListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("operator: encode deployment cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeDeploymentListCursor(encoded, filterDigest string) (deploymentListCursor, error) {
	if encoded == "" || len(encoded) > 2048 {
		return deploymentListCursor{}, fmt.Errorf("%w: cursor length is invalid", ErrInvalidDeploymentRead)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return deploymentListCursor{}, fmt.Errorf("%w: cursor encoding is invalid", ErrInvalidDeploymentRead)
	}
	var cursor deploymentListCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return deploymentListCursor{}, fmt.Errorf("%w: cursor document is invalid", ErrInvalidDeploymentRead)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return deploymentListCursor{}, fmt.Errorf("%w: cursor has trailing JSON", ErrInvalidDeploymentRead)
	}
	canonical, err := encodeDeploymentListCursor(cursor)
	_, offset := cursor.CreatedAt.Zone()
	if err != nil || canonical != encoded || cursor.Version != DeploymentReadSchemaVersion ||
		cursor.FilterDigest != filterDigest || cursor.CreatedAt.IsZero() || offset != 0 ||
		!validDeploymentIdentifier(cursor.DeploymentID, 256) {
		return deploymentListCursor{}, fmt.Errorf("%w: cursor does not match this query", ErrInvalidDeploymentRead)
	}
	return cursor, nil
}

func deploymentPositionAfter(createdAt time.Time, deploymentID string, cursorAt time.Time, cursorID string) bool {
	return createdAt.Before(cursorAt) || (createdAt.Equal(cursorAt) && deploymentID < cursorID)
}

func validDeploymentIdentifier(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value ||
		value == "." || value == ".." || strings.Contains(value, "/") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validDeploymentText(value string, maxBytes int, allowEmpty bool) bool {
	if len(value) > maxBytes || !utf8.ValidString(value) || (!allowEmpty && strings.TrimSpace(value) == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func isDeploymentExclusion(value string) bool {
	return value == rollout.ExcludedConflict || value == rollout.ExcludedMissingPackage ||
		value == rollout.ExcludedUnknownNode || value == rollout.ExcludedNoncompliant
}

func isDeploymentPauseKind(value string) bool {
	switch value {
	case store.DeploymentPausePromoteLocked, store.DeploymentPauseConflict,
		store.DeploymentPauseStaleRevision, store.DeploymentPauseBatchNotReady,
		store.DeploymentPauseMaterial, store.DeploymentPauseInvalidPlan,
		store.DeploymentPauseTargetChanged, store.DeploymentPauseMachineRetired:
		return true
	default:
		return false
	}
}

func isDeploymentState(state string) bool {
	return state == store.DeploymentRunning || state == store.DeploymentPaused || state == store.DeploymentFinished
}

func deploymentStateOrder(state string) int {
	for i, candidate := range deploymentStates {
		if state == candidate {
			return i
		}
	}
	return len(deploymentStates)
}

func utcTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}
