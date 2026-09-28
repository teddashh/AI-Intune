package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// Updates returns the native read model behind the Updates overview and the
// canary/stable submenus. Artifact entries remain metadata-only in this call.
func (c *Client) Updates(ctx context.Context, request operator.UpdateReadRequest) (operator.UpdateReadResult, error) {
	query, effectiveLimit, err := encodeUpdateReadQuery(request)
	if err != nil {
		return operator.UpdateReadResult{}, err
	}
	path := "/v1/operator/updates"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.UpdateReadResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.UpdateReadResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.UpdateReadResult{}, fmt.Errorf("operator client: updates returned HTTP %d", response.status)
	}
	if err := validateArtifactReadHeaders(response.header); err != nil {
		return operator.UpdateReadResult{}, err
	}
	var result operator.UpdateReadResult
	if err := decodeStrictJSONDocument(response.body, "updates", &result); err != nil {
		return operator.UpdateReadResult{}, err
	}
	if err := validateUpdateReadResult(result, request, effectiveLimit); err != nil {
		return operator.UpdateReadResult{}, err
	}
	return result, nil
}

func encodeUpdateReadQuery(request operator.UpdateReadRequest) (url.Values, int, error) {
	if request.Channel != "" && request.Channel != "canary" && request.Channel != "stable" {
		return nil, 0, errors.New("operator client: update channel must be canary or stable")
	}
	query, limit, err := encodeArtifactListQuery(operator.ArtifactListRequest{
		Status: request.Status, Version: request.Version, Limit: request.Limit, Cursor: request.Cursor,
	})
	if err != nil {
		return nil, 0, err
	}
	if request.Channel != "" {
		query.Set("channel", request.Channel)
	}
	return query, limit, nil
}

func validateUpdateReadResult(result operator.UpdateReadResult, request operator.UpdateReadRequest, limit int) error {
	if result.SchemaVersion != operator.UpdateReadSchemaVersion || result.Consistency != operator.UpdateReadConsistencyLive ||
		result.EvaluatedAt.IsZero() || result.Channels == nil {
		return errors.New("operator client: invalid updates schema or evaluation time")
	}
	if err := requireUTCArtifactTime(result.EvaluatedAt, "updates evaluated_at"); err != nil {
		return err
	}
	artifactRequest := operator.ArtifactListRequest{
		Status: request.Status, Version: request.Version, Limit: request.Limit, Cursor: request.Cursor,
	}
	if err := validateArtifactListResult(result.Artifacts, artifactRequest, limit); err != nil {
		return fmt.Errorf("operator client: updates artifact page: %w", err)
	}
	if !result.Artifacts.EvaluatedAt.Equal(result.EvaluatedAt) {
		return errors.New("operator client: updates carries inconsistent evaluation instants")
	}
	wantChannels := []string{"canary", "stable"}
	if request.Channel != "" {
		wantChannels = []string{request.Channel}
	}
	if len(result.Channels) != len(wantChannels) {
		return errors.New("operator client: updates channel selection is inconsistent")
	}
	for i, channel := range result.Channels {
		if channel.Name != wantChannels[i] || channel.Members == nil || channel.Previews == nil ||
			channel.MemberCount != len(channel.Members) || len(channel.Previews) != len(result.Artifacts.Items) {
			return fmt.Errorf("operator client: updates channel %d has inconsistent shape", i)
		}
		seen := make(map[string]bool, len(channel.Members))
		for _, member := range channel.Members {
			if !validArtifactClientText(member.MachineID, 256) || !validArtifactClientText(member.DisplayName, 256) ||
				seen[member.MachineID] ||
				(member.LastObservedVersion != nil && !validArtifactClientText(*member.LastObservedVersion, 128)) ||
				(member.LastObservedVersion != nil && member.LastObservationReceivedAt == nil) {
				return errors.New("operator client: updates contains invalid or duplicate member")
			}
			if member.LastObservationReceivedAt != nil {
				if err := requireUTCArtifactTime(*member.LastObservationReceivedAt, "updates member last_observation_received_at"); err != nil {
					return err
				}
				if member.LastObservationReceivedAt.After(result.EvaluatedAt) {
					return errors.New("operator client: updates member observation is after evaluated_at")
				}
			}
			seen[member.MachineID] = true
		}
		if channel.LatestDeployment != nil {
			if channel.LatestDeployment.Channel != channel.Name {
				return errors.New("operator client: latest deployment belongs to another channel")
			}
			if err := validateDeploymentSummary(*channel.LatestDeployment); err != nil {
				return fmt.Errorf("operator client: updates latest deployment: %w", err)
			}
		}
		for itemIndex, preview := range channel.Previews {
			if !equalArtifactSummary(preview.Artifact, result.Artifacts.Items[itemIndex]) {
				return errors.New("operator client: update preview artifact differs from artifact page")
			}
			if err := validateArtifactSummary(preview.Artifact, result.EvaluatedAt, false); err != nil {
				return fmt.Errorf("operator client: update artifact preview: %w", err)
			}
			if err := validateUpdatePreview(preview, channel.Name, channel.MemberCount, result.EvaluatedAt); err != nil {
				return err
			}
		}
	}
	return nil
}

func equalArtifactSummary(left, right operator.ArtifactSummary) bool {
	return left.ArtifactID == right.ArtifactID &&
		equalArtifactValue(left.Name, right.Name) &&
		equalArtifactValue(left.Version, right.Version) &&
		equalArtifactValue(left.SHA256, right.SHA256) &&
		equalArtifactValue(left.Digest, right.Digest) &&
		equalArtifactValue(left.SizeBytes, right.SizeBytes) &&
		equalArtifactValue(left.EnginesNode, right.EnginesNode) &&
		equalArtifactTime(left.FetchedAt, right.FetchedAt) &&
		left.Status == right.Status &&
		equalArtifactValue(left.Issue, right.Issue) &&
		left.DeploymentReferences == right.DeploymentReferences &&
		left.ActiveDeploymentReferences == right.ActiveDeploymentReferences &&
		equalArtifactTime(left.VerifiedAt, right.VerifiedAt)
}

func equalArtifactValue[T comparable](left, right *T) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalArtifactTime(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}

func validateUpdatePreview(preview operator.UpdateArtifactPreview, channel string, members int, evaluatedAt time.Time) error {
	eligible := preview.Artifact.Status == operator.ArtifactAvailableUnverified && preview.Artifact.Name != nil &&
		*preview.Artifact.Name == "openclaw" && preview.Artifact.Version != nil && preview.Artifact.SHA256 != nil &&
		preview.Artifact.EnginesNode != nil
	if !eligible {
		if preview.Plan != nil || preview.Promotion != nil || len(preview.Blockers) != 1 || preview.Blockers[0] != "artifact_not_available" {
			return errors.New("operator client: unavailable update artifact has rollout posture")
		}
		return nil
	}
	if preview.Plan == nil || preview.Plan.Impact < 0 || preview.Plan.Conflicts < 0 ||
		preview.Plan.MissingPackages < 0 || preview.Plan.UnknownNodes < 0 || preview.Plan.Unreachable < 0 ||
		preview.Plan.Unreachable > preview.Plan.Impact ||
		preview.Plan.Impact+preview.Plan.Conflicts+preview.Plan.MissingPackages+preview.Plan.UnknownNodes != members {
		return errors.New("operator client: update rollout plan is inconsistent")
	}
	wantBatches := 0
	if preview.Plan.Impact > 0 {
		wantBatches = (preview.Plan.Impact-1)/operator.DefaultDeploymentBatchSize + 1
	}
	if preview.Plan.TotalBatches != wantBatches {
		return errors.New("operator client: update rollout batches are inconsistent")
	}
	seenBlocker := make(map[string]bool, len(preview.Blockers))
	for _, blocker := range preview.Blockers {
		if blocker != "no_included_targets" && blocker != "stable_promotion_locked" || seenBlocker[blocker] {
			return errors.New("operator client: update preview has invalid blockers")
		}
		seenBlocker[blocker] = true
	}
	if (preview.Plan.Impact == 0) != seenBlocker["no_included_targets"] {
		return errors.New("operator client: update target blocker contradicts plan")
	}
	if channel == "canary" {
		if preview.Promotion != nil || seenBlocker["stable_promotion_locked"] {
			return errors.New("operator client: canary update carries stable promotion evidence")
		}
		return nil
	}
	if preview.Promotion == nil || preview.Promotion.Blockers == nil ||
		preview.Promotion.Allowed == seenBlocker["stable_promotion_locked"] {
		return errors.New("operator client: stable promotion evidence is inconsistent")
	}
	if err := validateDeploymentPromotionPreview(*preview.Promotion, evaluatedAt); err != nil {
		return err
	}
	return nil
}
