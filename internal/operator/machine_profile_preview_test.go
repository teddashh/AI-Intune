package operator

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineProfilePreviewAndReviewedPublication(t *testing.T) {
	service, _, _, manifestRequest := admittedOpenClawProfileService(t)
	request := machineProfileRequest(manifestRequest.Manifest, "reviewed-profile")
	preview, err := service.PreviewMachineProfile(t.Context(), MachineProfilePreviewRequest{Profile: request.Profile})
	if err != nil || preview.SchemaVersion != MachineProfilePreviewSchemaVersion ||
		preview.ProfileDigest == "" || preview.PreviewDigest == "" || preview.AlreadyPublished {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	request.ConfirmProfileID = request.Profile.ID
	request.ConfirmRevision = request.Profile.Revision
	request.PreviewDigest = preview.PreviewDigest
	result, err := service.PublishReviewedMachineProfile(t.Context(), request)
	if err != nil || result.Record.Digest != preview.ProfileDigest || result.Replayed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	again, err := service.PreviewMachineProfile(t.Context(), MachineProfilePreviewRequest{Profile: request.Profile})
	if err != nil || !again.AlreadyPublished || again.ProfileDigest != result.Record.Digest {
		t.Fatalf("again=%+v err=%v", again, err)
	}
}

func TestReviewedMachineProfileReplaysWithoutMaterial(t *testing.T) {
	service, _, dir, manifestRequest := admittedOpenClawProfileService(t)
	request := machineProfileRequest(manifestRequest.Manifest, "reviewed-profile-replay")
	preview, err := service.PreviewMachineProfile(t.Context(), MachineProfilePreviewRequest{Profile: request.Profile})
	if err != nil {
		t.Fatal(err)
	}
	request.ConfirmProfileID, request.ConfirmRevision, request.PreviewDigest =
		request.Profile.ID, request.Profile.Revision, preview.PreviewDigest
	first, err := service.PublishReviewedMachineProfile(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, manifestRequest.Manifest.Artifact.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replay, err := service.PublishReviewedMachineProfile(t.Context(), request)
	if err != nil || !replay.Replayed || replay.Record.Digest != first.Record.Digest {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestReviewedMachineProfileDurablyRejectsReviewMismatch(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	base := machineProfileRequest(manifestRequest.Manifest, "unused")
	preview, err := service.PreviewMachineProfile(t.Context(), MachineProfilePreviewRequest{Profile: base.Profile})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		code string
		edit func(*MachineProfilePublishRequest)
	}{
		{"confirmation", store.OperatorCodeMachineProfileConfirmationMismatch, func(r *MachineProfilePublishRequest) { r.ConfirmRevision++ }},
		{"preview", store.OperatorCodeMachineProfilePreviewStale, func(r *MachineProfilePublishRequest) { r.PreviewDigest = "sha256:bad" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.IdempotencyKey = "reviewed-profile-reject-" + test.name
			request.ConfirmProfileID, request.ConfirmRevision, request.PreviewDigest =
				request.Profile.ID, request.Profile.Revision, preview.PreviewDigest
			test.edit(&request)
			for attempt := 0; attempt < 2; attempt++ {
				_, err := service.PublishReviewedMachineProfile(t.Context(), request)
				var rejection *store.OperatorRequestError
				if !errors.As(err, &rejection) || rejection.Code != test.code || rejection.Replayed != (attempt == 1) {
					t.Fatalf("attempt=%d err=%+v", attempt, err)
				}
			}
			var count int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, request.IdempotencyKey).Scan(&count); err != nil || count != 1 {
				t.Fatalf("receipts=%d err=%v", count, err)
			}
		})
	}
}
