package operator

import (
	"encoding/json"
	"testing"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestAntigravityProfileAssignmentResolvesEachPlatform(t *testing.T) {
	service, st, _, _ := catalogManifestService(t)
	record := writeAntigravityStandardArtifact(t, service.artifactsDir, "1.2.14")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishStandardCatalogManifest(t.Context(), standardCatalogPublishRequest(preview, "antigravity-profile-manifest")); err != nil {
		t.Fatal(err)
	}
	profile := machineProfileRequest(preview.Manifest, "antigravity-profile")
	profile.Profile.ID = "antigravity-standard"
	if _, err := service.PublishMachineProfile(t.Context(), profile); err != nil {
		t.Fatal(err)
	}
	machines := []struct {
		name, osName, arch string
		want               appcatalog.Platform
	}{
		{"agy-linux-amd64", "Ubuntu 26.04 LTS", "x86_64", appcatalog.Platform{OS: "linux", Arch: "amd64"}},
		{"agy-linux-arm64", "Oracle Linux Server 9.7", "aarch64", appcatalog.Platform{OS: "linux", Arch: "arm64"}},
		{"agy-darwin-amd64", "macOS 15.1", "x86_64", appcatalog.Platform{OS: "darwin", Arch: "amd64"}},
		{"agy-darwin-arm64", "macOS 15.1", "arm64", appcatalog.Platform{OS: "darwin", Arch: "arm64"}},
		{"agy-windows-amd64", "Windows 11 Pro", "x86_64", appcatalog.Platform{OS: "windows", Arch: "amd64"}},
		{"agy-windows-arm64", "Windows 11 Pro", "aarch64", appcatalog.Platform{OS: "windows", Arch: "arm64"}},
	}
	for _, machine := range machines {
		t.Run(machine.name, func(t *testing.T) {
			machineID := enrollProfileAssignmentMachine(t, st, machine.name)
			if _, err := st.DB().Exec(`UPDATE machine_registry SET os=?,arch=? WHERE machine_id=?`,
				machine.osName, machine.arch, machineID); err != nil {
				t.Fatal(err)
			}
			assignmentPreview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
				MachineID: machineID, ProfileID: profile.Profile.ID, ProfileRevision: profile.Profile.Revision,
			})
			if err != nil || assignmentPreview.Target != machine.want || len(assignmentPreview.Packages) != 1 ||
				assignmentPreview.Packages[0].ResourceKind != agentadapter.ExecutorKindAntigravity {
				t.Fatalf("preview=%+v err=%v", assignmentPreview, err)
			}
			result, err := service.AssignMachineProfile(t.Context(),
				profileAssignmentRequest(machineID, profile, assignmentPreview, "assign-"+machine.name))
			if err != nil || len(result.Packages) != 1 {
				t.Fatalf("assign=%+v err=%v", result, err)
			}
			desired, err := st.DesiredState(result.Packages[0].DesiredID)
			if err != nil {
				t.Fatal(err)
			}
			var spec model.AntigravitySpec
			if err := json.Unmarshal([]byte(desired.Spec), &spec); err != nil ||
				spec.Kind != "antigravity" || spec.Version != "1.2.14" ||
				spec.TargetOS != machine.want.OS || spec.TargetArch != machine.want.Arch ||
				spec.BundleLayout != model.AntigravityBundleLayoutV1 || spec.Artifact == nil ||
				spec.Artifact.SHA256 != record.SHA256 {
				t.Fatalf("spec=%+v err=%v", spec, err)
			}
		})
	}
}
