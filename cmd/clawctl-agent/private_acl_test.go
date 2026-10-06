package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	windowsTestUserSID     = "S-1-5-21-1-2-3-1001"
	windowsTestOtherSID    = "S-1-5-21-1-2-3-1002"
	windowsTestEveryoneSID = "S-1-1-0"
	windowsTestSystemSID   = "S-1-5-18"
	windowsFileAllAccess   = 0x001F01FF
)

func TestWindowsNativeCredentialProtection(t *testing.T) {
	ownerAll := windowsPrivateACLView{
		OwnerSID:      windowsTestUserSID,
		DACLPresent:   true,
		DACLProtected: true,
		ACEs: []windowsPrivateACE{{
			Type: windowsAccessAllowedACEType, Mask: windowsGenericAll, SID: windowsTestUserSID,
		}},
	}

	t.Run("owner-only generic all", func(t *testing.T) {
		if err := inspectWindowsCredentialHandle(0, ownerAll, windowsTestUserSID, true); err != nil {
			t.Fatalf("owner-only GENERIC_ALL rejected: %v", err)
		}
	})
	t.Run("mapped file all access", func(t *testing.T) {
		view := ownerAll
		view.ACEs = []windowsPrivateACE{{
			Type: windowsAccessAllowedACEType, Mask: windowsFileAllAccess, SID: windowsTestUserSID,
		}}
		if err := inspectWindowsCredentialHandle(windowsFileAttributeArchive(), view, windowsTestUserSID, true); err != nil {
			t.Fatalf("FILE_ALL_ACCESS rejected: %v", err)
		}
	})
	t.Run("owner read write specific rights", func(t *testing.T) {
		view := ownerAll
		view.ACEs = []windowsPrivateACE{{
			Type: windowsAccessAllowedACEType,
			Mask: windowsGenericRead | windowsGenericWrite,
			SID:  windowsTestUserSID,
		}}
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, true); err != nil {
			t.Fatalf("GENERIC_READ|GENERIC_WRITE rejected: %v", err)
		}
	})
	t.Run("owner read only may load", func(t *testing.T) {
		view := ownerAll
		view.ACEs = []windowsPrivateACE{{
			Type: windowsAccessAllowedACEType, Mask: windowsFileReadData, SID: windowsTestUserSID,
		}}
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err != nil {
			t.Fatalf("owner-read credential rejected on load: %v", err)
		}
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, true); err == nil {
			t.Fatal("owner-read credential accepted as a written private file")
		}
	})
	t.Run("reparse point", func(t *testing.T) {
		if err := inspectWindowsCredentialHandle(windowsFileAttributeReparsePoint, ownerAll, windowsTestUserSID, false); err == nil {
			t.Fatal("reparse point accepted")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if err := inspectWindowsCredentialHandle(windowsFileAttributeDirectory, ownerAll, windowsTestUserSID, false); err == nil {
			t.Fatal("directory accepted")
		}
	})
	t.Run("directory junction", func(t *testing.T) {
		attrs := uint32(windowsFileAttributeDirectory | windowsFileAttributeReparsePoint)
		if err := inspectWindowsCredentialHandle(attrs, ownerAll, windowsTestUserSID, true); err == nil {
			t.Fatal("directory junction accepted")
		}
	})
	t.Run("everyone ace", func(t *testing.T) {
		view := ownerAll
		view.ACEs = append(append([]windowsPrivateACE(nil), view.ACEs...), windowsPrivateACE{
			Type: windowsAccessAllowedACEType, Mask: windowsGenericRead, SID: windowsTestEveryoneSID,
		})
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("Everyone ACE accepted")
		}
	})
	t.Run("system extra ace", func(t *testing.T) {
		view := ownerAll
		view.ACEs = append(append([]windowsPrivateACE(nil), view.ACEs...), windowsPrivateACE{
			Type: windowsAccessAllowedACEType, Mask: windowsGenericAll, SID: windowsTestSystemSID,
		})
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, true); err == nil {
			t.Fatal("SYSTEM ACE accepted")
		}
	})
	t.Run("owner mismatch", func(t *testing.T) {
		view := ownerAll
		view.OwnerSID = windowsTestOtherSID
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("other-user owner accepted")
		}
	})
	t.Run("inherited ace", func(t *testing.T) {
		view := ownerAll
		view.ACEs[0].Flags = windowsInheritedACE
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("inherited ACE accepted")
		}
	})
	t.Run("inherit-only ace", func(t *testing.T) {
		view := ownerAll
		view.ACEs[0].Flags = windowsInheritOnlyACE
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("inherit-only ACE accepted")
		}
	})
	t.Run("deny ace", func(t *testing.T) {
		view := ownerAll
		view.ACEs = []windowsPrivateACE{
			{Type: windowsAccessDeniedACEType, Mask: windowsGenericAll, SID: windowsTestEveryoneSID},
			{Type: windowsAccessAllowedACEType, Mask: windowsGenericAll, SID: windowsTestUserSID},
		}
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("deny ACE accepted")
		}
	})
	t.Run("unprotected dacl", func(t *testing.T) {
		view := ownerAll
		view.DACLProtected = false
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("unprotected DACL accepted")
		}
	})
	t.Run("missing dacl", func(t *testing.T) {
		view := ownerAll
		view.DACLPresent = false
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("missing DACL accepted")
		}
	})
	t.Run("empty acl", func(t *testing.T) {
		view := ownerAll
		view.ACEs = nil
		if err := inspectWindowsCredentialHandle(0, view, windowsTestUserSID, false); err == nil {
			t.Fatal("empty DACL accepted")
		}
	})
	t.Run("blank current user", func(t *testing.T) {
		if err := inspectWindowsCredentialHandle(0, ownerAll, "", false); err == nil {
			t.Fatal("blank current-user SID accepted")
		}
	})
}

func windowsFileAttributeArchive() uint32 { return 0x00000020 }

func TestPrivateCredentialCopyNamesCurrentState(t *testing.T) {
	msg := errPrivateRegularFile.Error()
	if !strings.Contains(msg, "private regular file") {
		t.Fatalf("missing current state: %q", msg)
	}
	for _, banned := range []string{"DACL", "ACL", "chmod", "POSIX", "reparse", "mode bits", "尚未", "TODO"} {
		if strings.Contains(msg, banned) {
			t.Fatalf("operator copy leaked %q: %q", banned, msg)
		}
	}
	unixNotice := enrollmentStoredNotice("machine-1", `C:\clawctl\agent.json`, "linux")
	if !strings.Contains(unixNotice, "（0600）") || !strings.Contains(unixNotice, "已報到") {
		t.Fatalf("unix enrollment notice=%q", unixNotice)
	}
	windowsNotice := enrollmentStoredNotice("machine-1", `C:\clawctl\agent.json`, "windows")
	if !strings.Contains(windowsNotice, "僅目前使用者可讀寫") {
		t.Fatalf("windows enrollment notice=%q", windowsNotice)
	}
	if strings.Contains(windowsNotice, "0600") {
		t.Fatalf("windows enrollment notice kept POSIX mode: %q", windowsNotice)
	}
	for _, banned := range []string{"DACL", "ACL", "chmod", "reparse"} {
		if strings.Contains(windowsNotice, banned) {
			t.Fatalf("windows enrollment notice leaked %q: %q", banned, windowsNotice)
		}
	}
}

func TestEnrollmentReceiptAuthenticatesCheckinAndReadinessAfterRestart(t *testing.T) {
	isolateAgentHome(t)
	cfg, err := enrollmentConfig("http://hub.invalid", model.EnrollResponse{
		SchemaVersion: model.SchemaVersion,
		MachineID:     "machine-1", AgentToken: "agent-token",
		CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 600,
		SettingsDigest: testSettingsDigest(90, 600),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("restart reread failed: %v", err)
	}
	if loaded.MachineID != "machine-1" || loaded.AgentToken != "agent-token" || !loaded.JobsEnabled {
		t.Fatalf("restart config=%+v", loaded)
	}

	enabled := true
	received := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	server := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+loaded.AgentToken {
			t.Fatalf("path %s auth=%q", r.URL.Path, got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/checkins":
			_ = json.NewEncoder(w).Encode(model.CheckinResponse{
				ReceivedAt: received, CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 600,
				SettingsDigest: loaded.EnrollmentSettingsDigest,
			})
		case "/v1/agent/readiness":
			_ = json.NewEncoder(w).Encode(model.AgentReadinessResponse{
				MachineID: loaded.MachineID, LastCheckinReceivedAt: cliTimePtr(received),
				AgentStartedAt: cliTimePtr(received), AgentVersion: "v1", JobsEnabled: &enabled,
				DeviceSyncV1: &enabled, IdentityReceivedAt: cliTimePtr(received),
				IdentityMeasuredAt: cliTimePtr(received),
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))

	var checkin model.CheckinResponse
	if err := postJSON(t.Context(), server.URL+"/v1/checkins", loaded.AgentToken, model.Checkin{
		SchemaVersion: model.SchemaVersion, AgentVersion: "v1",
	}, &checkin); err != nil {
		t.Fatalf("check-in with restarted credential: %v", err)
	}
	if checkin.ReceivedAt != received {
		t.Fatalf("check-in receipt=%+v", checkin)
	}

	var readiness model.AgentReadinessResponse
	if err := doJSON(t.Context(), http.MethodGet, server.URL+"/v1/agent/readiness", loaded.AgentToken, nil, &readiness); err != nil {
		t.Fatalf("readiness with restarted credential: %v", err)
	}
	if pending := readinessPending(readiness, loaded.MachineID, "v1", received, false); pending != "" {
		t.Fatalf("readiness pending=%q receipt=%+v", pending, readiness)
	}
}
