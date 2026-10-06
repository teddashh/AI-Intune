package operator

import (
	"errors"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/rollout"
)

const (
	UpdateReadSchemaVersion   = 5
	UpdateReadConsistencyLive = "live"
)

var ErrInvalidUpdateRead = errors.New("operator: invalid update read request")

// UpdateReadRequest binds artifact pagination to the channel whose rollout
// posture is being inspected. An empty channel returns both managed channels.
type UpdateReadRequest struct {
	Channel string
	Status  ArtifactReadStatus
	Version string
	Limit   int
	Cursor  string
}

// UpdateMachineSummary is an explicit allowlist. In particular it omits host,
// address, unix-user, notes, and credential material from the registry row.
type UpdateMachineSummary struct {
	MachineID                 string     `json:"machine_id"`
	DisplayName               string     `json:"display_name"`
	LastObservedVersion       *string    `json:"last_observed_version"`
	LastObservationReceivedAt *time.Time `json:"last_observation_received_at"`
}

type UpdatePlanSummary struct {
	Impact          int `json:"impact"`
	Conflicts       int `json:"conflicts"`
	MissingPackages int `json:"missing_packages"`
	UnknownNodes    int `json:"unknown_nodes"`
	Noncompliant    int `json:"noncompliant"`
	Unreachable     int `json:"unreachable"`
	TotalBatches    int `json:"total_batches"`
}

type UpdateArtifactPreview struct {
	Artifact  ArtifactSummary             `json:"artifact"`
	Plan      *UpdatePlanSummary          `json:"plan"`
	Promotion *DeploymentPromotionPreview `json:"promotion"`
	Blockers  []string                    `json:"blockers"`
}

type UpdateChannelSummary struct {
	Name             string                  `json:"name"`
	MemberCount      int                     `json:"member_count"`
	Members          []UpdateMachineSummary  `json:"members"`
	LatestDeployment *DeploymentSummary      `json:"latest_deployment"`
	Previews         []UpdateArtifactPreview `json:"previews"`
}

type UpdateReadResult struct {
	SchemaVersion int                    `json:"schema_version"`
	Consistency   string                 `json:"consistency"`
	EvaluatedAt   time.Time              `json:"evaluated_at"`
	Artifacts     ArtifactListResult     `json:"artifacts"`
	Channels      []UpdateChannelSummary `json:"channels"`
}

// Updates composes the artifact catalog, channel membership, last-observed
// versions, latest deployment, and per-artifact rollout posture at one caller-
// supplied evaluation instant. The artifact list remains metadata-only; this
// endpoint must not imply that tarball bytes were hashed by an overview read.
func (s *Service) Updates(request UpdateReadRequest, evaluatedAt time.Time) (UpdateReadResult, error) {
	if s == nil || s.store == nil || evaluatedAt.IsZero() {
		return UpdateReadResult{}, fmt.Errorf("%w: store and evaluated_at are required", ErrInvalidUpdateRead)
	}
	if request.Channel != "" && request.Channel != "canary" && request.Channel != "stable" {
		return UpdateReadResult{}, fmt.Errorf("%w: channel must be canary or stable", ErrInvalidUpdateRead)
	}
	evaluatedAt = evaluatedAt.UTC()
	artifacts, err := s.ListArtifacts(ArtifactListRequest{
		Status: request.Status, Version: request.Version, Limit: request.Limit, Cursor: request.Cursor,
	}, evaluatedAt)
	if err != nil {
		if errors.Is(err, ErrInvalidArtifactRead) {
			return UpdateReadResult{}, fmt.Errorf("%w: %v", ErrInvalidUpdateRead, err)
		}
		return UpdateReadResult{}, err
	}
	result := UpdateReadResult{
		SchemaVersion: UpdateReadSchemaVersion, Consistency: UpdateReadConsistencyLive,
		EvaluatedAt: evaluatedAt, Artifacts: artifacts, Channels: []UpdateChannelSummary{},
	}
	channels := []string{"canary", "stable"}
	if request.Channel != "" {
		channels = []string{request.Channel}
	}
	for _, channel := range channels {
		block, err := s.updateChannel(channel, artifacts.Items, evaluatedAt)
		if err != nil {
			return UpdateReadResult{}, err
		}
		result.Channels = append(result.Channels, block)
	}
	return result, nil
}

func (s *Service) updateChannel(channel string, artifacts []ArtifactSummary, evaluatedAt time.Time) (UpdateChannelSummary, error) {
	members, err := s.store.MachinesInChannel(channel)
	if err != nil {
		return UpdateChannelSummary{}, err
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.MachineID)
	}
	installs, err := s.store.LatestOpenClawInstallObservationsAt(ids, evaluatedAt)
	if err != nil {
		return UpdateChannelSummary{}, err
	}
	block := UpdateChannelSummary{
		Name: channel, MemberCount: len(members), Members: make([]UpdateMachineSummary, 0, len(members)),
		Previews: make([]UpdateArtifactPreview, 0, len(artifacts)),
	}
	for _, member := range members {
		projected := UpdateMachineSummary{MachineID: member.MachineID, DisplayName: member.DisplayName}
		if observation, ok := installs[member.MachineID]; ok && !observation.ReceivedAt.IsZero() {
			receivedAt := observation.ReceivedAt.UTC()
			projected.LastObservationReceivedAt = &receivedAt
			if install := observation.Install; install != nil &&
				validDeploymentIdentifier(install.RunningDirVersion, 128) {
				value := install.RunningDirVersion
				projected.LastObservedVersion = &value
			}
		}
		block.Members = append(block.Members, projected)
	}
	deployments, err := s.ListDeployments(DeploymentListRequest{Channel: channel, Limit: 1}, evaluatedAt)
	if err != nil {
		return UpdateChannelSummary{}, err
	}
	if len(deployments.Items) == 1 {
		latest := deployments.Items[0]
		block.LatestDeployment = &latest
	}
	facts, err := s.store.DeploymentFacts(members, evaluatedAt)
	if err != nil {
		return UpdateChannelSummary{}, err
	}
	// NodeVersion is machine-supplied observation data. Normalize its trust
	// boundary once before the per-artifact loop: malformed or oversized input is
	// unknown evidence, and must not be repeatedly split/parsed for every catalog
	// item on an Updates page.
	for i := range facts {
		if !validDeploymentIdentifier(facts[i].NodeVersion, 128) {
			facts[i].NodeVersion = ""
		}
	}
	for _, item := range artifacts {
		preview := UpdateArtifactPreview{Artifact: item, Blockers: []string{}}
		if item.Status != ArtifactAvailableUnverified || item.Name == nil || *item.Name != "openclaw" ||
			item.Version == nil || item.SHA256 == nil || item.EnginesNode == nil {
			preview.Blockers = append(preview.Blockers, "artifact_not_available")
			block.Previews = append(block.Previews, preview)
			continue
		}
		plan := rollout.Plan(facts, *item.EnginesNode, DefaultDeploymentBatchSize)
		preview.Plan = &UpdatePlanSummary{
			Impact: plan.Impact, Conflicts: plan.Conflicts, MissingPackages: plan.MissingPackages,
			UnknownNodes: plan.UnknownNodes, Noncompliant: plan.Noncompliant,
			Unreachable: plan.Unreachable, TotalBatches: plan.TotalBatches,
		}
		if plan.Impact == 0 {
			preview.Blockers = append(preview.Blockers, "no_included_targets")
		}
		if channel == "stable" {
			decision, err := s.store.PreviewStableOpenClawPromotion(*item.Version, *item.SHA256, evaluatedAt)
			if err != nil {
				return UpdateChannelSummary{}, err
			}
			promotion := projectDeploymentPromotion(decision)
			preview.Promotion = &promotion
			if !promotion.Allowed {
				preview.Blockers = append(preview.Blockers, "stable_promotion_locked")
			}
		}
		block.Previews = append(block.Previews, preview)
	}
	return block, nil
}
