package operator

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Exercise reachable enrollment and publication paths, including a profile
// published for amd64 but assigned to arm64. No corrupt catalog rows are needed
// to distinguish a resolver failure from an unsupported machine platform.
func TestProfileAssignmentRejectionsKeepTheirCauseThroughPreviewApplyAndReplay(t *testing.T) {
	t.Run("stored Windows identity", checkProfileAssignmentUsesStoredWindowsIdentityInsteadOfLinuxPackages)
	t.Run("Windows identity after Linux pin", checkWindowsIdentityCannotReuseOrExtendALinuxPinnedAssignment)
	for _, test := range []struct {
		name       string
		os         string
		arch       string
		unenrolled bool
		revision   int64
		code       string
		sentinel   error
		status     int
		cause      string
	}{
		{"never enrolled", "", "", true, 1, store.OperatorCodeMachinePlatformUnknown, store.ErrMachinePlatformUnknown, http.StatusConflict, "作業系統或架構"},
		{"missing OS", "", "amd64", false, 1, store.OperatorCodeMachinePlatformUnknown, store.ErrMachinePlatformUnknown, http.StatusConflict, "作業系統或架構"},
		{"missing architecture", "macOS 15.1", "", false, 1, store.OperatorCodeMachinePlatformUnknown, store.ErrMachinePlatformUnknown, http.StatusConflict, "作業系統或架構"},
		{"malformed architecture", "Linux", " amd64", false, 1, store.OperatorCodeMachinePlatformUnknown, store.ErrMachinePlatformUnknown, http.StatusConflict, "作業系統或架構"},
		{"Mac arm with Linux-only profile", "macOS 15.1", "arm64", false, 1, store.OperatorCodeMachineProfileUnresolvable, store.ErrMachineProfileUnresolvable, http.StatusBadRequest, "解析"},
		{"Mac x86 with Linux-only profile", "macOS 15.1", "x86_64", false, 1, store.OperatorCodeMachineProfileUnresolvable, store.ErrMachineProfileUnresolvable, http.StatusBadRequest, "解析"},
		{"unsupported architecture", "Linux", "riscv64", false, 1, store.OperatorCodeMachinePlatformUnsupported, store.ErrMachinePlatformUnsupported, http.StatusConflict, "套件"},
		{"Windows 11 x86", "Windows 11 Pro", "x86_64", false, 1, store.OperatorCodeMachineProfileUnresolvable, store.ErrMachineProfileUnresolvable, http.StatusBadRequest, "解析"},
		{"windows tailscale amd64", "windows", "amd64", false, 1, store.OperatorCodeMachineProfileUnresolvable, store.ErrMachineProfileUnresolvable, http.StatusBadRequest, "解析"},
		{"Windows Server arm", "Windows Server 2022", "aarch64", false, 1, store.OperatorCodeMachineProfileUnresolvable, store.ErrMachineProfileUnresolvable, http.StatusBadRequest, "解析"},
		{"Microsoft Windows 10", "Microsoft Windows 10", "x86_64", false, 1, store.OperatorCodeMachineProfileUnresolvable, store.ErrMachineProfileUnresolvable, http.StatusBadRequest, "解析"},
		{"missing profile revision", "Ubuntu 26.04 LTS", "x86_64", false, 2, store.OperatorCodeMachineProfileNotFound, store.ErrMachineProfileNotFound, http.StatusNotFound, "profile 版本"},
		{"profile platform mismatch", "Linux", "aarch64", false, 1, store.OperatorCodeMachineProfileUnresolvable, store.ErrMachineProfileUnresolvable, http.StatusBadRequest, "解析"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, st, _, record := catalogManifestService(t)
			manifest := catalogManifestRequest(record, "rejection-manifest")
			manifest.Manifest.Platforms = []appcatalog.Platform{{OS: "linux", Arch: "amd64"}}
			if _, err := service.PublishCatalogManifest(t.Context(), manifest); err != nil {
				t.Fatal(err)
			}
			profile := machineProfileRequest(manifest.Manifest, "rejection-profile")
			if _, err := service.PublishMachineProfile(t.Context(), profile); err != nil {
				t.Fatal(err)
			}
			machineID, token, err := st.CreateEnrollTokenFor("profile-rejection-target", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if !test.unenrolled {
				if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
					SchemaVersion: model.SchemaVersion, Hostname: "profile-rejection-target",
					UnixUser: "profile-test", OS: test.os, Arch: test.arch, AgentVersion: "test",
				}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			machine, err := st.GetMachine(machineID)
			if err != nil || machine.OS != test.os || machine.Arch != test.arch {
				t.Fatalf("stored identity OS=%q arch=%q err=%v", machine.OS, machine.Arch, err)
			}

			assertRejection := func(err error, audited, replayed bool) *store.OperatorRequestError {
				t.Helper()
				var rejection *store.OperatorRequestError
				if !errors.As(err, &rejection) || rejection.Code != test.code ||
					rejection.Audited != audited || rejection.Replayed != replayed || !errors.Is(err, test.sentinel) {
					t.Fatalf("rejection=%+v err=%v; want code=%s audited=%t replayed=%t", rejection, err, test.code, audited, replayed)
				}
				status, code, detail := HTTPError(err)
				if status != test.status || code != test.code || detail != rejection.Detail {
					t.Fatalf("HTTP error=(%d,%q,%q)", status, code, detail)
				}
				if !replayed && (!strings.Contains(detail, test.cause) || detail == code) {
					t.Fatalf("operator message does not identify the cause: %q", detail)
				}
				return rejection
			}
			_, err = service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
				MachineID: machineID, ProfileID: profile.Profile.ID, ProfileRevision: test.revision,
			})
			preview := assertRejection(err, false, false)
			request := MachineProfileAssignmentRequest{
				MachineID: machineID, ProfileID: profile.Profile.ID, ProfileRevision: test.revision,
				Reason: "inspect assignment rejection", IdempotencyKey: "rejection-assignment", Actor: verifiedDeploymentActor(),
			}
			_, err = service.AssignMachineProfile(t.Context(), request)
			applied := assertRejection(err, true, false)
			if preview.Detail != applied.Detail {
				t.Fatalf("preview=%q apply=%q", preview.Detail, applied.Detail)
			}
			var outcome, code, detail, auditDetail string
			if err := st.DB().QueryRow(`SELECT outcome,error_code,error_detail FROM operator_idempotency
				WHERE idempotency_key=?`, request.IdempotencyKey).Scan(&outcome, &code, &detail); err != nil {
				t.Fatal(err)
			}
			if err := st.DB().QueryRow(`SELECT detail FROM audit_log WHERE idempotency_key=?`, request.IdempotencyKey).Scan(&auditDetail); err != nil {
				t.Fatal(err)
			}
			if outcome != "rejected" || code != test.code || auditDetail != detail ||
				!strings.HasSuffix(detail, "；"+applied.Detail) || !strings.Contains(detail, "code="+test.code) {
				t.Fatalf("receipt=(%q,%q,%q) audit=%q", outcome, code, detail, auditDetail)
			}
			_, err = service.AssignMachineProfile(t.Context(), request)
			replayed := assertRejection(err, true, true)
			if !strings.Contains(replayed.Detail, test.code) || !strings.Contains(replayed.Detail, "當時") {
				t.Fatalf("replay must describe the historical decision: %q", replayed.Detail)
			}
			for _, table := range []string{"machine_profile_assignments", "machine_profile_assignment_packages", "desired_state", "jobs"} {
				if count := countOperatorRows(t, st, table); count != 0 {
					t.Errorf("rejected assignment wrote %d rows to %s", count, table)
				}
			}
		})
	}
}

func TestProfileAssignmentUsesStoredMacIdentityInsteadOfCallingItUnknown(t *testing.T) {
	service, st, _, record := catalogManifestService(t)
	manifest := catalogManifestRequest(record, "observed-mac-identity-manifest")
	manifest.Manifest.Platforms = []appcatalog.Platform{{OS: "linux", Arch: "amd64"}}
	if _, err := service.PublishCatalogManifest(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	profile := machineProfileRequest(manifest.Manifest, "observed-mac-identity-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profile); err != nil {
		t.Fatal(err)
	}

	machineID, token, err := st.CreateEnrollTokenFor("observed-mac-identity", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, Hostname: "observed-mac-identity",
		UnixUser: "profile-test", Arch: "arm64", AgentVersion: "old",
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		Identity: model.Identity{Hostname: "observed-mac-identity", OS: "macOS 15.7", Arch: "arm64"},
	}, now); err != nil {
		t.Fatal(err)
	}

	_, err = service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profile.Profile.ID, ProfileRevision: profile.Profile.Revision,
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeMachineProfileUnresolvable ||
		!errors.Is(err, store.ErrMachineProfileUnresolvable) {
		t.Fatalf("stored mac identity rejection=%+v err=%v; want %s, not a false request for another agent report",
			rejection, err, store.OperatorCodeMachineProfileUnresolvable)
	}
	if strings.Contains(rejection.Detail, "agent 回報") {
		t.Fatalf("Hub 已保存 macOS identity evidence，卻仍建議等待 agent 回報：%q", rejection.Detail)
	}
}

func checkProfileAssignmentUsesStoredWindowsIdentityInsteadOfLinuxPackages(t *testing.T) {
	service, st, _, record := catalogManifestService(t)
	manifest := catalogManifestRequest(record, "observed-windows-identity-manifest")
	manifest.Manifest.Platforms = []appcatalog.Platform{{OS: "linux", Arch: "amd64"}}
	if _, err := service.PublishCatalogManifest(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	profile := machineProfileRequest(manifest.Manifest, "observed-windows-identity-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profile); err != nil {
		t.Fatal(err)
	}

	machineID, token, err := st.CreateEnrollTokenFor("observed-windows-identity", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, Hostname: "observed-windows-identity",
		UnixUser: "profile-test", Arch: "amd64", AgentVersion: "old",
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		Identity: model.Identity{Hostname: "observed-windows-identity", OS: "Windows 11 Pro", Arch: "amd64"},
	}, now); err != nil {
		t.Fatal(err)
	}

	_, err = service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profile.Profile.ID, ProfileRevision: profile.Profile.Revision,
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeMachineProfileUnresolvable ||
		!errors.Is(err, store.ErrMachineProfileUnresolvable) {
		t.Fatalf("stored windows identity rejection=%+v err=%v; want %s, not a linux package pin",
			rejection, err, store.OperatorCodeMachineProfileUnresolvable)
	}
	if strings.Contains(rejection.Detail, "agent 回報") {
		t.Fatalf("Hub 已保存 Windows identity evidence，卻仍建議等待 agent 回報：%q", rejection.Detail)
	}
	for _, table := range []string{"machine_profile_assignments", "machine_profile_assignment_packages", "desired_state", "jobs"} {
		if count := countOperatorRows(t, st, table); count != 0 {
			t.Errorf("Windows identity wrote %d rows to %s", count, table)
		}
	}
}
