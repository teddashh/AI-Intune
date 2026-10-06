package store

import (
	"encoding/json"
	"fmt"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

// MachineMayDownloadArtifact proves that an authenticated machine currently
// owns a non-terminal job whose immutable desired-state document names the
// exact content-addressed artifact. A machine bearer token is not an operator
// catalog credential: knowing another digest must not be enough to download it.
func (s *Store) MachineMayDownloadArtifact(machineID, sha256Hex string) (bool, error) {
	if machineID == "" || len(sha256Hex) != 64 {
		return false, nil
	}
	for _, c := range sha256Hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false, nil
		}
	}
	rows, err := s.rdb.Query(`
SELECT d.resource_kind,d.resource_id,d.spec
  FROM jobs j
  JOIN desired_state d ON d.desired_id=j.desired_id
 WHERE j.machine_id=?
   AND j.artifact_digest=?
   AND j.state NOT IN (?,?,?,?,?)
 ORDER BY j.created_at DESC,j.job_id DESC`, machineID, "sha256:"+sha256Hex,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention)
	if err != nil {
		return false, fmt.Errorf("store: inspect machine artifact grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var resourceKind, resourceID, raw string
		if err := rows.Scan(&resourceKind, &resourceID, &raw); err != nil {
			return false, fmt.Errorf("store: scan machine artifact grant: %w", err)
		}
		// Both supported jobs carry this typed version/artifact envelope.
		var spec struct {
			Kind     string             `json:"kind"`
			Version  string             `json:"version"`
			Artifact *model.ArtifactRef `json:"artifact"`
		}
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			return false, fmt.Errorf("store: machine artifact grant has invalid desired state: %w", err)
		}
		supported := resourceKind == agentadapter.ExecutorKindOpenClaw || resourceKind == agentadapter.ExecutorKindNodeRuntime
		if !supported || resourceID != resourceKind || spec.Kind != resourceKind ||
			spec.Artifact == nil || spec.Artifact.SHA256 != sha256Hex ||
			spec.Artifact.URL != "/v1/artifacts/"+sha256Hex || spec.Artifact.Size < 0 {
			return false, fmt.Errorf("store: machine artifact grant contradicts desired state")
		}
		return true, nil
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: iterate machine artifact grants: %w", err)
	}
	return false, nil
}
