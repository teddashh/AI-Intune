package operator

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	MachineConnectSchemaVersion = 1
	MachineConnectListenerLimit = 20
	MachineConnectMaxTextBytes  = 4096
)

var ErrInvalidMachineConnectAudit = errors.New("operator: invalid machine connect audit request")

type MachineConnectAuditOutcome string

const (
	MachineConnectAuditDetailUnavailable     MachineConnectAuditOutcome = "detail_unavailable"
	MachineConnectAuditProjectionUnavailable MachineConnectAuditOutcome = "projection_unavailable"
	MachineConnectAuditAddressUnavailable    MachineConnectAuditOutcome = "address_unavailable"
	MachineConnectAuditRedirected            MachineConnectAuditOutcome = "redirected"
)

type MachineConnectAuditRequest struct {
	Outcome   MachineConnectAuditOutcome
	MachineID string
	Connect   *MachineConnectResult
	Reason    string
	Actor     Actor
}

type MachineConnectAddressSource string

const (
	MachineConnectAddressNone              MachineConnectAddressSource = "none"
	MachineConnectAddressListener          MachineConnectAddressSource = "listener"
	MachineConnectAddressIdentityTailscale MachineConnectAddressSource = "identity_tailscale"
)

type machineConnectTextField string

const (
	machineConnectURL        machineConnectTextField = "url"
	machineConnectWhy        machineConnectTextField = "why"
	machineConnectBind       machineConnectTextField = "bat.bind"
	machineConnectArgv       machineConnectTextField = "bat.argv"
	machineConnectListener   machineConnectTextField = "bat.listener"
	machineConnectRegistryIP machineConnectTextField = "registry_ip"
)

var machineConnectTextPolicies = map[machineConnectTextField]evidenceTextPolicy{
	machineConnectURL:        {maxBytes: 2048, policy: boundedTextStrict},
	machineConnectWhy:        {maxBytes: MachineConnectMaxTextBytes, policy: boundedTextBlock},
	machineConnectBind:       {maxBytes: 256, policy: boundedTextStrict},
	machineConnectArgv:       {maxBytes: MachineConnectMaxTextBytes, policy: boundedTextBlock},
	machineConnectListener:   {maxBytes: 64, policy: boundedTextStrict},
	machineConnectRegistryIP: {maxBytes: 64, policy: boundedTextStrict},
}

type MachineConnectDisclosure struct {
	ListenerLimit               int                     `json:"listener_limit"`
	MaxTextBytes                int                     `json:"max_text_bytes"`
	Producer                    MachineEvidenceProducer `json:"producer"`
	IndependentVerifier         bool                    `json:"independent_verifier"`
	CoordinatesIncluded         bool                    `json:"coordinates_included"`
	CredentialsIncluded         bool                    `json:"credentials_included"`
	HubProxiesConnection        bool                    `json:"hub_proxies_connection"`
	ButtonAuditCoversCopiedURLs bool                    `json:"button_audit_covers_copied_urls"`
	RegistryIPUsedForURL        bool                    `json:"registry_ip_used_for_url"`
	ArgvParsedByHub             bool                    `json:"argv_parsed_by_hub"`
	DisplayTextKeywordScanning  bool                    `json:"display_text_keyword_scanning"`
}

type MachineConnectResult struct {
	SchemaVersion     int                          `json:"schema_version"`
	EvaluatedAt       time.Time                    `json:"evaluated_at"`
	MachineID         string                       `json:"machine_id"`
	DisplayName       string                       `json:"display_name"`
	Available         bool                         `json:"available"`
	URL               *EvidenceText                `json:"url"`
	Why               EvidenceText                 `json:"why"`
	AddressSource     MachineConnectAddressSource  `json:"address_source"`
	RegistryIP        *EvidenceText                `json:"registry_ip"`
	RegistryIPInvalid bool                         `json:"registry_ip_invalid"`
	Identity          MachineConnectIdentity       `json:"identity"`
	BAT               MachineConnectBATObservation `json:"bat"`
	Disclosure        MachineConnectDisclosure     `json:"disclosure"`
}

type MachineConnectIdentity struct {
	Observed   bool                  `json:"observed"`
	Decoded    bool                  `json:"decoded"`
	Invalid    bool                  `json:"invalid"`
	ObservedAt *MachineEvidenceClock `json:"observed_at"`
}

type MachineConnectBATObservation struct {
	Observed   bool                  `json:"observed"`
	Decoded    bool                  `json:"decoded"`
	Invalid    bool                  `json:"invalid"`
	ObservedAt *MachineEvidenceClock `json:"observed_at"`
	Value      *MachineConnectBAT    `json:"value"`
}

type MachineConnectBAT struct {
	Running     bool                      `json:"running"`
	Port        int                       `json:"port"`
	Bind        *EvidenceText             `json:"bind"`
	Argv        *EvidenceText             `json:"argv"`
	ListenAddrs MachineConnectAddressPage `json:"listen_addrs"`
}

type MachineConnectAddressPage struct {
	Total     int            `json:"total"`
	Invalid   int            `json:"invalid"`
	Truncated bool           `json:"truncated"`
	Items     []EvidenceText `json:"items"`
}

// MachineConnect projects actionable BAT coordinates only from an in-process
// MachineDetail snapshot. HTTP-decoded detail deliberately has no raw source,
// so it cannot be upgraded into a coordinate-bearing response by an adapter.
func (s *Service) MachineConnect(detail MachineDetailResult) (MachineConnectResult, error) {
	if detail.detail == nil {
		return MachineConnectResult{}, errors.New("operator: machine connect source unavailable")
	}
	raw := detail.detail
	if detail.Item.MachineID == "" || raw.Machine.MachineID != detail.Item.MachineID ||
		detail.EvaluatedAt.IsZero() {
		return MachineConnectResult{}, errors.New("operator: machine connect source identity is inconsistent")
	}
	result := MachineConnectResult{
		SchemaVersion: MachineConnectSchemaVersion,
		EvaluatedAt:   detail.EvaluatedAt.UTC(),
		MachineID:     detail.Item.MachineID,
		DisplayName:   detail.Item.DisplayName,
		AddressSource: MachineConnectAddressNone,
		Disclosure: MachineConnectDisclosure{
			ListenerLimit: MachineConnectListenerLimit, MaxTextBytes: MachineConnectMaxTextBytes,
			Producer: MachineEvidenceProducer{
				Kind: MachineEvidenceProducerObserverAgent, MachineID: detail.Item.MachineID,
				DisplayName: detail.Item.DisplayName, Authority: MachineEvidenceAuthorityMachineBearer,
			},
			IndependentVerifier: false, CoordinatesIncluded: true, CredentialsIncluded: false,
			HubProxiesConnection: false, ButtonAuditCoversCopiedURLs: false,
			RegistryIPUsedForURL: false, ArgvParsedByHub: false, DisplayTextKeywordScanning: false,
		},
	}

	result.Identity = projectMachineConnectIdentity(*raw, detail.EvaluatedAt)
	identityIP := ""
	if result.Identity.Decoded && !result.Identity.Invalid && raw.Identity != nil && raw.Identity.TailscaleIP != "" {
		if ip := net.ParseIP(raw.Identity.TailscaleIP); ip != nil {
			identityIP = ip.String()
		} else {
			result.Identity.Invalid = true
		}
	}
	if raw.Connect.RegistryIP != "" {
		if ip := net.ParseIP(raw.Connect.RegistryIP); ip != nil {
			projected := projectMachineConnectText(machineConnectRegistryIP, ip.String())
			result.RegistryIP = &projected
		} else {
			result.RegistryIPInvalid = true
		}
	}

	result.BAT = projectMachineConnectBAT(raw.Connect, detail.EvaluatedAt)
	if !result.BAT.Observed {
		result.Why = projectMachineConnectText(machineConnectWhy,
			"尚無 bat-server 觀測")
		return result, nil
	}
	if !result.BAT.Decoded || result.BAT.Invalid || result.BAT.Value == nil {
		result.Why = projectMachineConnectText(machineConnectWhy,
			"bat-server observation 無法形成可信的 typed 連線座標")
		return result, nil
	}

	bat := raw.Connect.BAT
	bat.ListenAddrs = make([]string, 0, len(result.BAT.Value.ListenAddrs.Items))
	for _, address := range result.BAT.Value.ListenAddrs.Items {
		bat.ListenAddrs = append(bat.ListenAddrs, address.Text)
	}
	connectURL, why := model.ConnectURL(bat, identityIP)
	if connectURL == "" {
		if why == "" {
			why = "這台給不出可以連過去的位址"
		}
		result.Why = projectMachineConnectText(machineConnectWhy, why)
		return result, nil
	}
	if !validMachineConnectURL(connectURL, bat.Port) {
		result.BAT.Invalid = true
		result.Why = projectMachineConnectText(machineConnectWhy,
			"bat-server observation 產生了不合法的連線座標")
		return result, nil
	}
	projectedURL := projectMachineConnectText(machineConnectURL, connectURL)
	result.URL, result.Available = &projectedURL, true
	result.Why = projectMachineConnectText(machineConnectWhy, "")
	result.AddressSource = machineConnectAddressSource(connectURL, bat.ListenAddrs)
	return result, nil
}

// RecordMachineConnectAudit owns the durable identity of a Connect button
// attempt. It records only the browser-mediated redirect decision: a copied
// BAT URL still bypasses this path, and the Hub never proxies the connection.
func (s *Service) RecordMachineConnectAudit(request MachineConnectAuditRequest) error {
	entry := auditFromActor(request.Actor)
	entry.Action = store.AuditConnect
	machineID, fallbackSubject := machineConnectAuditIdentity(request.MachineID)
	entry.MachineID, entry.Subject = machineID, fallbackSubject

	switch request.Outcome {
	case MachineConnectAuditDetailUnavailable:
		if request.Connect != nil || request.Reason != "" {
			return fmt.Errorf("%w: detail-unavailable shape", ErrInvalidMachineConnectAudit)
		}
		entry.Detail = "operator machine detail unavailable"
	case MachineConnectAuditProjectionUnavailable:
		if request.Connect != nil || request.Reason != "" {
			return fmt.Errorf("%w: projection-unavailable shape", ErrInvalidMachineConnectAudit)
		}
		entry.Detail = "operator machine connect projection unavailable"
	case MachineConnectAuditAddressUnavailable:
		if err := validateMachineConnectAuditResult(request, false); err != nil {
			return err
		}
		entry.MachineID = request.Connect.MachineID
		entry.Subject = request.Connect.DisplayName
		entry.Reason = request.Reason
		entry.Detail = request.Connect.Why.Text
	case MachineConnectAuditRedirected:
		if err := validateMachineConnectAuditResult(request, true); err != nil {
			return err
		}
		entry.MachineID = request.Connect.MachineID
		entry.Subject = request.Connect.DisplayName + " → " + request.Connect.URL.Text
		entry.Reason = request.Reason
		entry.OK = true
	default:
		return fmt.Errorf("%w: unknown outcome %q", ErrInvalidMachineConnectAudit, request.Outcome)
	}
	return s.store.RecordAudit(entry)
}

func machineConnectAuditIdentity(raw string) (machineID, subject string) {
	if validateMachineReadText("machine connect machine_id", raw, 256) != nil {
		return "", "machine connect"
	}
	return raw, raw
}

func validateMachineConnectAuditResult(request MachineConnectAuditRequest, redirected bool) error {
	result := request.Connect
	if result == nil || result.SchemaVersion != MachineConnectSchemaVersion || result.EvaluatedAt.IsZero() ||
		result.MachineID != request.MachineID ||
		validateMachineReadText("machine connect machine_id", result.MachineID, 256) != nil ||
		validateMachineReadText("machine connect display_name", result.DisplayName, 256) != nil {
		return fmt.Errorf("%w: result identity", ErrInvalidMachineConnectAudit)
	}
	if !redirected {
		if result.Available || result.URL != nil ||
			validateMachineReadText("machine connect why", result.Why.Text, MachineConnectMaxTextBytes) != nil {
			return fmt.Errorf("%w: unavailable result", ErrInvalidMachineConnectAudit)
		}
		return nil
	}
	if !result.Available || result.URL == nil || result.Why.Text != "" || result.BAT.Value == nil ||
		validateMachineReadText("machine connect URL", result.URL.Text, 2048) != nil ||
		!validMachineConnectURL(result.URL.Text, result.BAT.Value.Port) {
		return fmt.Errorf("%w: redirect result", ErrInvalidMachineConnectAudit)
	}
	return nil
}

func projectMachineConnectIdentity(detail store.Detail, evaluatedAt time.Time) MachineConnectIdentity {
	result := MachineConnectIdentity{Observed: detail.IdentityObserved, Decoded: detail.IdentityDecoded}
	if detail.IdentityObservedAt != nil {
		if validMachineObservationClock(detail.IdentityObservedAt.MeasuredAt,
			detail.IdentityObservedAt.ReceivedAt, evaluatedAt) {
			result.ObservedAt = machineDetailClock(detail.IdentityObservedAt)
		} else {
			result.Invalid = true
		}
	}
	if result.Observed != (detail.IdentityObservedAt != nil) || result.Decoded && !result.Observed ||
		result.Decoded != (detail.Identity != nil) {
		result.Invalid = true
	}
	return result
}

func projectMachineConnectBAT(raw store.ConnectInfo, evaluatedAt time.Time) MachineConnectBATObservation {
	result := MachineConnectBATObservation{Observed: raw.Observed, Decoded: raw.Decoded}
	if raw.Observed {
		if validMachineObservationClock(raw.MeasuredAt, raw.ReceivedAt, evaluatedAt) {
			result.ObservedAt = &MachineEvidenceClock{MeasuredAt: raw.MeasuredAt, ReceivedAt: raw.ReceivedAt}
		} else {
			result.Invalid = true
		}
	}
	if raw.Observed != !raw.MeasuredAt.IsZero() || raw.Observed != !raw.ReceivedAt.IsZero() ||
		raw.Decoded && !raw.Observed {
		result.Invalid = true
	}
	if !raw.Decoded {
		result.Invalid = raw.Observed
		return result
	}
	value := MachineConnectBAT{
		Running: raw.BAT.Running, Port: raw.BAT.Port,
		Bind: projectOptionalMachineConnectText(machineConnectBind, raw.BAT.Bind),
		Argv: projectOptionalMachineConnectText(machineConnectArgv, raw.BAT.Argv),
		ListenAddrs: MachineConnectAddressPage{
			Total: len(raw.BAT.ListenAddrs), Items: make([]EvidenceText, 0, min(len(raw.BAT.ListenAddrs), MachineConnectListenerLimit)),
		},
	}
	if raw.BAT.Port < 0 || raw.BAT.Port > 65535 {
		result.Invalid = true
	}
	seen := map[string]bool{}
	for _, rawAddress := range raw.BAT.ListenAddrs {
		ip := net.ParseIP(rawAddress)
		if ip == nil || seen[ip.String()] {
			value.ListenAddrs.Invalid++
			result.Invalid = true
			continue
		}
		seen[ip.String()] = true
		if len(value.ListenAddrs.Items) == MachineConnectListenerLimit {
			continue
		}
		value.ListenAddrs.Items = append(value.ListenAddrs.Items,
			projectMachineConnectText(machineConnectListener, ip.String()))
	}
	value.ListenAddrs.Truncated = len(value.ListenAddrs.Items)+value.ListenAddrs.Invalid < value.ListenAddrs.Total
	if value.ListenAddrs.Truncated {
		// A URL must never depend on an address omitted from its disclosure.
		result.Invalid = true
	}
	result.Value = &value
	return result
}

func validMachineConnectURL(value string, port int) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || net.ParseIP(parsed.Hostname()) == nil {
		return false
	}
	parsedPort, err := strconv.Atoi(parsed.Port())
	return err == nil && port > 0 && port <= 65535 && parsedPort == port
}

func machineConnectAddressSource(value string, listeners []string) MachineConnectAddressSource {
	parsed, _ := url.Parse(value)
	host := net.ParseIP(parsed.Hostname())
	for _, listener := range listeners {
		ip := net.ParseIP(listener)
		if ip != nil && !ip.IsUnspecified() && ip.Equal(host) {
			return MachineConnectAddressListener
		}
	}
	return MachineConnectAddressIdentityTailscale
}

func projectMachineConnectText(field machineConnectTextField, value string) EvidenceText {
	policy, ok := machineConnectTextPolicies[field]
	if !ok {
		panic("operator: missing machine connect text policy for " + string(field))
	}
	return projectEvidenceText(policy, value)
}

func projectOptionalMachineConnectText(field machineConnectTextField, value string) *EvidenceText {
	if value == "" {
		return nil
	}
	projected := projectMachineConnectText(field, value)
	return &projected
}
