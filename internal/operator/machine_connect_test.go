package operator

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func machineConnectFixture(t *testing.T, registryIP string) (*Service, *store.Store, string, time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	machineID, token, err := st.CreateEnrollTokenFor("connect-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "connect-machine", OS: "linux", Arch: "amd64", UnixUser: "operator-test",
		TailscaleIP: registryIP,
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return New(st), st, machineID, now
}

func recordMachineConnectObservation(t *testing.T, st *store.Store, machineID string,
	identity model.Identity, bat model.BAT, measuredAt, receivedAt time.Time,
) {
	t.Helper()
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: measuredAt,
		Identity: identity, BAT: bat,
	}, receivedAt); err != nil {
		t.Fatal(err)
	}
}

func TestMachineConnectProjectsBoundedCoordinatesAndTwoClocks(t *testing.T) {
	service, st, machineID, now := machineConnectFixture(t, "100.64.0.9")
	measuredAt, receivedAt := now.Add(-2*time.Minute), now.Add(-time.Minute)
	recordMachineConnectObservation(t, st, machineID, model.Identity{
		Hostname: "connect-machine", OS: "linux", Arch: "amd64", UnixUser: "operator-test",
		TailscaleIP: "100.64.0.10",
	}, model.BAT{
		Running: true, Port: 9876, Bind: "tailscale\u202e",
		Argv:        "/opt/bat\n" + strings.Repeat("x", MachineConnectMaxTextBytes) + "\x1b[31m",
		ListenAddrs: []string{"100.64.0.10"},
	}, measuredAt, receivedAt)
	detail, err := service.MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.MachineConnect(detail)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != MachineConnectSchemaVersion || !result.EvaluatedAt.Equal(now) ||
		result.MachineID != machineID || result.DisplayName != "connect-machine" ||
		!result.Available || result.URL == nil || result.URL.Text != "https://100.64.0.10:9876" ||
		result.AddressSource != MachineConnectAddressListener || result.Why.Text != "" {
		t.Fatalf("connect result=%+v", result)
	}
	if !result.Identity.Observed || !result.Identity.Decoded || result.Identity.Invalid ||
		result.Identity.ObservedAt == nil || !result.Identity.ObservedAt.MeasuredAt.Equal(measuredAt) ||
		!result.Identity.ObservedAt.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("identity=%+v", result.Identity)
	}
	if !result.BAT.Observed || !result.BAT.Decoded || result.BAT.Invalid || result.BAT.ObservedAt == nil ||
		result.BAT.Value == nil || result.BAT.Value.Port != 9876 ||
		len(result.BAT.Value.ListenAddrs.Items) != 1 || result.BAT.Value.ListenAddrs.Invalid != 0 {
		t.Fatalf("bat=%+v", result.BAT)
	}
	if result.BAT.Value.Bind == nil || !strings.Contains(result.BAT.Value.Bind.Text, "�") ||
		result.BAT.Value.Argv == nil || !result.BAT.Value.Argv.Truncated ||
		strings.ContainsRune(result.BAT.Value.Argv.Text, '\x1b') ||
		!strings.Contains(result.BAT.Value.Argv.Text, "\n") {
		t.Fatalf("bounded BAT text bind=%+v argv=%+v", result.BAT.Value.Bind, result.BAT.Value.Argv)
	}
	if result.RegistryIP == nil || result.RegistryIP.Text != "100.64.0.9" || result.RegistryIPInvalid ||
		!result.Disclosure.CoordinatesIncluded || result.Disclosure.CredentialsIncluded ||
		result.Disclosure.HubProxiesConnection || result.Disclosure.ButtonAuditCoversCopiedURLs ||
		result.Disclosure.RegistryIPUsedForURL || result.Disclosure.ArgvParsedByHub ||
		result.Disclosure.DisplayTextKeywordScanning || result.Disclosure.IndependentVerifier {
		t.Fatalf("registry/disclosure result=%+v", result)
	}
}

func TestMachineConnectNeverFallsBackToEnrollmentTimeRegistryIP(t *testing.T) {
	service, st, machineID, now := machineConnectFixture(t, "100.64.0.9")
	recordMachineConnectObservation(t, st, machineID, model.Identity{
		Hostname: "connect-machine", OS: "linux", Arch: "amd64", UnixUser: "operator-test",
	}, model.BAT{
		Running: true, Port: 9876, Bind: "0.0.0.0", ListenAddrs: []string{"0.0.0.0"},
	}, now.Add(-time.Minute), now.Add(-time.Minute))
	detail, err := service.MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.MachineConnect(detail)
	if err != nil {
		t.Fatal(err)
	}
	if result.Available || result.URL != nil || result.AddressSource != MachineConnectAddressNone ||
		!strings.Contains(result.Why.Text, "Tailscale") || result.Disclosure.RegistryIPUsedForURL {
		t.Fatalf("stale registry IP became a coordinate: %+v", result)
	}
}

func TestMachineConnectKeepsUndecodableAndMalformedEvidenceUnavailable(t *testing.T) {
	t.Run("undecodable", func(t *testing.T) {
		service, st, machineID, now := machineConnectFixture(t, "100.64.0.9")
		recordMachineConnectObservation(t, st, machineID, model.Identity{TailscaleIP: "100.64.0.10"},
			model.BAT{Running: true, Port: 9876, ListenAddrs: []string{"100.64.0.10"}},
			now.Add(-time.Minute), now.Add(-time.Minute))
		if _, err := st.DB().Exec(`UPDATE observed_state SET payload='{' WHERE machine_id=? AND kind=?`,
			machineID, store.KindBAT); err != nil {
			t.Fatal(err)
		}
		detail, err := service.MachineDetail(machineID, now)
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.MachineConnect(detail)
		if err != nil || !result.BAT.Observed || result.BAT.Decoded || !result.BAT.Invalid ||
			result.BAT.Value != nil || result.Available || result.URL != nil ||
			!strings.Contains(result.Why.Text, "typed") {
			t.Fatalf("undecodable result=%+v err=%v", result, err)
		}
	})

	t.Run("malformed listener", func(t *testing.T) {
		service, st, machineID, now := machineConnectFixture(t, "100.64.0.9")
		recordMachineConnectObservation(t, st, machineID, model.Identity{TailscaleIP: "100.64.0.10"},
			model.BAT{Running: true, Port: 9876, ListenAddrs: []string{"not-an-ip"}},
			now.Add(-time.Minute), now.Add(-time.Minute))
		detail, err := service.MachineDetail(machineID, now)
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.MachineConnect(detail)
		if err != nil || !result.BAT.Invalid || result.BAT.Value == nil ||
			result.BAT.Value.ListenAddrs.Invalid != 1 || result.Available || result.URL != nil {
			t.Fatalf("malformed result=%+v err=%v", result, err)
		}
	})
}

func TestMachineConnectRequiresInProcessDetailSource(t *testing.T) {
	service, _, machineID, now := machineConnectFixture(t, "100.64.0.9")
	detail, err := service.MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var decoded MachineDetailResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if result, err := service.MachineConnect(decoded); err == nil || result.URL != nil {
		t.Fatalf("decoded detail escalated into connect coordinates: result=%+v err=%v", result, err)
	}
}

func TestMachineConnectAuditOwnsEveryButtonOutcomeShape(t *testing.T) {
	tests := []struct {
		name        string
		outcome     MachineConnectAuditOutcome
		withResult  bool
		available   bool
		reason      string
		wantOK      bool
		wantSubject string
		wantDetail  string
	}{
		{"detail unavailable", MachineConnectAuditDetailUnavailable, false, false, "", false,
			"machine-id", "operator machine detail unavailable"},
		{"projection unavailable", MachineConnectAuditProjectionUnavailable, false, false, "", false,
			"machine-id", "operator machine connect projection unavailable"},
		{"address unavailable", MachineConnectAuditAddressUnavailable, true, false, "inspect BAT", false,
			"connect-machine", "尚無 bat-server 觀測"},
		{"redirected", MachineConnectAuditRedirected, true, true, "open console", true,
			"connect-machine → https://100.64.0.10:9876", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, st, machineID, now := machineConnectFixture(t, "100.64.0.9")
			if test.name == "detail unavailable" || test.name == "projection unavailable" {
				machineID = "machine-id"
			}
			var result *MachineConnectResult
			if test.withResult {
				if test.available {
					recordMachineConnectObservation(t, st, machineID, model.Identity{
						TailscaleIP: "100.64.0.10",
					}, model.BAT{
						Running: true, Port: 9876, ListenAddrs: []string{"100.64.0.10"},
					}, now.Add(-time.Minute), now.Add(-time.Minute))
				}
				detail, err := service.MachineDetail(machineID, now)
				if err != nil {
					t.Fatal(err)
				}
				projected, err := service.MachineConnect(detail)
				if err != nil {
					t.Fatal(err)
				}
				result = &projected
			}
			actor := Actor{
				SourceAddr: "100.64.0.10", WhoNode: "console-node", WhoUser: "operator@example.com",
				AuthSubject: "tailscale-user:1", AuthNodeID: "node-1", AuthCapability: "operate",
				AuthMethod: "tailscale-localapi", AuthDecision: "authorized", SourceKind: SourceKindWeb,
			}
			err := service.RecordMachineConnectAudit(MachineConnectAuditRequest{
				Outcome: test.outcome, MachineID: machineID, Connect: result,
				Reason: test.reason, Actor: actor,
			})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := st.Audit("", 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("connect audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.Action != store.AuditConnect || entry.MachineID != machineID ||
				entry.Subject != test.wantSubject || entry.Reason != test.reason || entry.OK != test.wantOK ||
				entry.Detail != test.wantDetail || entry.IdempotencyKey != "" || entry.RequestDigest != "" ||
				entry.SourceAddr != actor.SourceAddr || entry.WhoNode != actor.WhoNode ||
				entry.WhoUser != actor.WhoUser || entry.AuthSubject != actor.AuthSubject ||
				entry.AuthNodeID != actor.AuthNodeID || entry.AuthCapability != actor.AuthCapability ||
				entry.AuthMethod != actor.AuthMethod || entry.AuthDecision != actor.AuthDecision ||
				entry.SourceKind != SourceKindWeb {
				t.Fatalf("connect audit shape=%+v", entry)
			}
		})
	}
}

func TestMachineConnectAuditRejectsImpossibleShapesWithoutWriting(t *testing.T) {
	service, st, machineID, now := machineConnectFixture(t, "100.64.0.9")
	detail, err := service.MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := service.MachineConnect(detail)
	if err != nil {
		t.Fatal(err)
	}
	recordMachineConnectObservation(t, st, machineID, model.Identity{TailscaleIP: "100.64.0.10"},
		model.BAT{Running: true, Port: 9876, ListenAddrs: []string{"100.64.0.10"}},
		now.Add(-time.Minute), now.Add(-time.Minute))
	detail, err = service.MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	available, err := service.MachineConnect(detail)
	if err != nil {
		t.Fatal(err)
	}
	mismatch := available
	mismatch.MachineID = "another-machine"
	tampered := available
	tamperedURL := *available.URL
	tamperedURL.Text = "https://example.com:9876"
	tampered.URL = &tamperedURL

	tests := []MachineConnectAuditRequest{
		{Outcome: MachineConnectAuditOutcome("free-form"), MachineID: machineID},
		{Outcome: MachineConnectAuditDetailUnavailable, MachineID: machineID, Connect: &unavailable},
		{Outcome: MachineConnectAuditProjectionUnavailable, MachineID: machineID, Reason: "not parsed here"},
		{Outcome: MachineConnectAuditAddressUnavailable, MachineID: machineID},
		{Outcome: MachineConnectAuditAddressUnavailable, MachineID: machineID, Connect: &available},
		{Outcome: MachineConnectAuditRedirected, MachineID: machineID, Connect: &unavailable},
		{Outcome: MachineConnectAuditRedirected, MachineID: machineID, Connect: &mismatch},
		{Outcome: MachineConnectAuditRedirected, MachineID: machineID, Connect: &tampered},
	}
	for index, request := range tests {
		if err := service.RecordMachineConnectAudit(request); !errors.Is(err, ErrInvalidMachineConnectAudit) {
			t.Errorf("invalid request %d err=%v", index, err)
		}
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("impossible Connect shapes wrote audit=%+v err=%v", entries, err)
	}
}

func TestMachineConnectAuditGeneralizesAnInvalidPathIdentity(t *testing.T) {
	service, st, _, _ := machineConnectFixture(t, "100.64.0.9")
	raw := strings.Repeat("x", 257)
	if err := service.RecordMachineConnectAudit(MachineConnectAuditRequest{
		Outcome: MachineConnectAuditDetailUnavailable, MachineID: raw,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 || entries[0].MachineID != "" ||
		entries[0].Subject != "machine connect" || strings.Contains(entries[0].Subject, raw) {
		t.Fatalf("invalid path identity audit=%+v err=%v", entries, err)
	}
}
