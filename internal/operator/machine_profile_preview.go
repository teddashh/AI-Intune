package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

const MachineProfilePreviewSchemaVersion = 1

type MachineProfilePreviewRequest struct {
	Profile appcatalog.MachineProfile `json:"profile"`
}

type MachineProfilePreviewResult struct {
	SchemaVersion    int                       `json:"schema_version"`
	Profile          appcatalog.MachineProfile `json:"profile"`
	ProfileDigest    string                    `json:"profile_digest"`
	AlreadyPublished bool                      `json:"already_published"`
	PreviewedAt      time.Time                 `json:"previewed_at"`
	PreviewDigest    string                    `json:"preview_digest"`
}

func (s *Service) PreviewMachineProfile(ctx context.Context,
	request MachineProfilePreviewRequest,
) (MachineProfilePreviewResult, error) {
	if s == nil || s.store == nil || ctx == nil || appcatalog.ValidateProfile(request.Profile) != nil {
		return MachineProfilePreviewResult{}, ErrInvalidMachineProfilePublication
	}
	raw, _ := json.Marshal(request.Profile)
	profile, err := appcatalog.ParseProfile(raw)
	if err != nil {
		return MachineProfilePreviewResult{}, ErrInvalidMachineProfilePublication
	}
	if err := s.verifyMachineProfileMaterial(ctx, profile); err != nil {
		return MachineProfilePreviewResult{}, err
	}
	digest := machineProfileContentDigest(profile)
	result := MachineProfilePreviewResult{
		SchemaVersion: MachineProfilePreviewSchemaVersion, Profile: profile, ProfileDigest: digest,
		PreviewedAt: time.Now().UTC().Truncate(time.Second), PreviewDigest: machineProfilePreviewDigest(profile),
	}
	if existing, existingErr := s.store.MachineProfile(profile.ID, profile.Revision); existingErr == nil {
		if existing.Digest != digest {
			return MachineProfilePreviewResult{}, machineProfileRejection(store.OperatorCodeMachineProfileConflict)
		}
		result.AlreadyPublished = true
	} else if !errors.Is(existingErr, store.ErrNotFound) {
		return MachineProfilePreviewResult{}, existingErr
	}
	return result, nil
}

func machineProfileContentDigest(profile appcatalog.MachineProfile) string {
	raw, err := json.Marshal(profile)
	if err != nil {
		return ""
	}
	canonical, err := appcatalog.ParseProfile(raw)
	if err != nil {
		return ""
	}
	raw, _ = json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func machineProfilePreviewDigest(profile appcatalog.MachineProfile) string {
	raw, _ := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		ProfileDigest string `json:"profile_digest"`
	}{MachineProfilePreviewSchemaVersion, machineProfileContentDigest(profile)})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
