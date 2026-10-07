// Package operatorauth authenticates the human/operator plane with Tailscale's
// destination-scoped LocalAPI identity and application capabilities.
//
// It deliberately does not inspect HTTP identity headers. The current Hub is
// reached directly on its tailnet address, so Forwarded, X-Forwarded-* and
// Tailscale-* are all caller-controlled bytes. Only tailscaled's LocalAPI is an
// authority at this boundary.
package operatorauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/version"
)

const (
	defaultResolveTimeout         = 3 * time.Second
	AuthMethodLocalAccountSession = "local-account-session"
	AuthMethodLocalAPI            = "tailscale-localapi-app-cap"
	minimumDaemonVersion          = "1.100.0"
)

// Permission is one independently granted operator capability. These are not
// hierarchical in code: an administrator grant must explicitly include view,
// operate and admin if that person needs all three.
type Permission uint8

const (
	View Permission = iota + 1
	Operate
	Admin
)

func (p Permission) String() string {
	switch p {
	case View:
		return "view"
	case Operate:
		return "operate"
	case Admin:
		return "admin"
	default:
		return "unknown"
	}
}

// CapabilityNames are three fixed, parameter-free capability keys. Keeping
// them separate avoids asking the application to reinterpret opaque policy
// parameters as a role hierarchy.
type CapabilityNames struct {
	View    string
	Operate string
	Admin   string
}

var (
	domainLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	capabilitySlug     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

// NamesForPrefix validates <owned-domain>/cap/<application> and derives the
// exact capability keys understood by this binary.
func NamesForPrefix(prefix string) (CapabilityNames, error) {
	if prefix == "" || prefix != strings.TrimSpace(prefix) || strings.ToLower(prefix) != prefix {
		return CapabilityNames{}, errors.New("operator capability prefix must be a lowercase name without whitespace")
	}
	parts := strings.Split(prefix, "/")
	if len(parts) != 3 || parts[1] != "cap" || !capabilitySlug.MatchString(parts[2]) {
		return CapabilityNames{}, fmt.Errorf("operator capability prefix %q must be <owned-domain>/cap/<application>", prefix)
	}
	domain := parts[0]
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return CapabilityNames{}, fmt.Errorf("operator capability domain %q must be a controlled domain and cannot be a single label", domain)
	}
	for _, label := range labels {
		if !domainLabelPattern.MatchString(label) {
			return CapabilityNames{}, fmt.Errorf("operator capability domain %q format is invalid", domain)
		}
	}
	if domain == "tailscale.com" || strings.HasSuffix(domain, ".tailscale.com") ||
		domain == "tailscale.io" || strings.HasSuffix(domain, ".tailscale.io") {
		return CapabilityNames{}, fmt.Errorf("operator capability domain %q is a reserved Tailscale namespace", domain)
	}
	return CapabilityNames{
		View: prefix + "-view", Operate: prefix + "-operate", Admin: prefix + "-admin",
	}, nil
}

// Config contains only trusted server configuration. Destination is the
// literal address on which the Hub operator plane listens; it must never be
// derived from the request Host header.
type Config struct {
	Destination      netip.Addr
	CapabilityPrefix string
	ResolveTimeout   time.Duration
}

// ResolverStatus is the small, security-relevant subset of LocalAPI status.
// Preflight must prove not only a sufficiently new daemon, but also that the
// configured destination belongs to this node and tailscaled is actually in
// the Running state.
type ResolverStatus struct {
	Version      string
	BackendState string
	TailscaleIPs []netip.Addr
}

// Resolver is the stable portion of Tailscale LocalAPI used here. The narrow
// interface keeps tests off the real tailscaled socket.
type Resolver interface {
	Status(context.Context) (ResolverStatus, error)
	WhoIsForIP(context.Context, string, netip.Addr) (*apitype.WhoIsResponse, error)
}

type localAPIResolver struct{ client *local.Client }

func (r *localAPIResolver) Status(ctx context.Context) (ResolverStatus, error) {
	status, err := r.client.StatusWithoutPeers(ctx)
	if err != nil {
		return ResolverStatus{}, err
	}
	if status == nil {
		return ResolverStatus{}, errors.New("Tailscale LocalAPI returned an empty status")
	}
	return ResolverStatus{
		Version: status.Version, BackendState: status.BackendState,
		TailscaleIPs: append([]netip.Addr(nil), status.TailscaleIPs...),
	}, nil
}

func (r *localAPIResolver) WhoIsForIP(ctx context.Context, remoteAddr string,
	destination netip.Addr,
) (*apitype.WhoIsResponse, error) {
	return r.client.WhoIsForIP(ctx, remoteAddr, destination)
}

type Authorizer struct {
	destination netip.Addr
	names       CapabilityNames
	timeout     time.Duration
	resolver    Resolver
}

// New constructs the production adapter using Tailscale's official LocalAPI
// client. UseSocketOnly prevents platform fallback paths from silently turning
// a missing local daemon into a different authority source.
func New(config Config) (*Authorizer, error) {
	return NewWithResolver(config, &localAPIResolver{client: &local.Client{UseSocketOnly: true}})
}

func NewWithResolver(config Config, resolver Resolver) (*Authorizer, error) {
	if resolver == nil {
		return nil, errors.New("operator auth resolver cannot be nil")
	}
	destination := config.Destination
	if !destination.IsValid() || destination.Zone() != "" || destination.IsUnspecified() ||
		destination.Unmap().IsLoopback() || !tsaddr.IsTailscaleIP(destination) {
		return nil, fmt.Errorf("operator auth destination %q must be an explicit Tailscale listener IP", destination)
	}
	names, err := NamesForPrefix(config.CapabilityPrefix)
	if err != nil {
		return nil, err
	}
	timeout := config.ResolveTimeout
	if timeout < 0 {
		return nil, errors.New("operator auth resolve timeout cannot be negative")
	}
	if timeout == 0 {
		timeout = defaultResolveTimeout
	}
	return &Authorizer{destination: destination, names: names, timeout: timeout, resolver: resolver}, nil
}

type DecisionCode string

const (
	Authorized               DecisionCode = "AUTHORIZED"
	Unauthenticated          DecisionCode = "OPERATOR_IDENTITY_REQUIRED"
	HumanPrincipalRequired   DecisionCode = "HUMAN_PRINCIPAL_REQUIRED"
	CapabilityRequired       DecisionCode = "OPERATOR_CAPABILITY_REQUIRED"
	AuthSourceUnavailable    DecisionCode = "AUTH_SOURCE_UNAVAILABLE"
	AuthConfigurationInvalid DecisionCode = "AUTH_CONFIGURATION_INVALID"
)

// Decision is safe to use for a stable HTTP denial contract. Cause is kept
// separately for server logs and must not be serialized to clients.
type Decision struct {
	Allowed    bool
	HTTPStatus int
	Code       DecisionCode
	Detail     string
	Principal  Principal
	cause      error
}

func (d Decision) Cause() error { return d.cause }

// Principal describes what Tailscale actually proved: a source node and the
// tailnet user owning/logged into that node. It is not a fresh assertion about
// the person currently at the keyboard.
type Principal struct {
	SourceAddr           string
	NodeStableID         string
	DeviceName           string
	TailnetUserID        string
	TailnetUserLogin     string
	TailnetDisplayName   string
	AuthMethod           string
	AuthorizedCapability string
	GrantedCapabilities  CapabilityNames
}

func (p Principal) StableSubject() string {
	if p.TailnetUserID == "" {
		return ""
	}
	if p.AuthMethod == AuthMethodLocalAccountSession {
		return "local-user:" + strings.TrimPrefix(p.TailnetUserID, "local:")
	}
	return "tailscale-user:" + p.TailnetUserID
}

func (p Principal) Has(permission Permission) bool {
	switch permission {
	case View:
		return p.GrantedCapabilities.View != ""
	case Operate:
		return p.GrantedCapabilities.Operate != ""
	case Admin:
		return p.GrantedCapabilities.Admin != ""
	default:
		return false
	}
}

// MatchesPeer reconciles the adapter-declared source address with the actual
// peer address of the connection.
func (p Principal) MatchesPeer(remoteAddr string) bool {
	declared, declaredOK := remoteHost(p.SourceAddr)
	peer, peerOK := remoteHost(remoteAddr)
	return declaredOK && peerOK && declared == peer
}

// PermissionLabel is descriptive, not inheritance. It lists every exact
// capability Tailscale returned for this source-to-destination connection.
func (p Principal) PermissionLabel() string {
	var granted []string
	for _, permission := range []Permission{View, Operate, Admin} {
		if p.Has(permission) {
			granted = append(granted, permission.String())
		}
	}
	if len(granted) == 0 {
		return "no operator capabilities"
	}
	return strings.Join(granted, ",")
}

func (p Principal) Attribution() string {
	device := p.DeviceName
	if device == "" {
		device = firstNonEmpty(p.SourceAddr, "unknown device")
	}
	user := firstNonEmpty(p.TailnetUserLogin, p.TailnetDisplayName, "unknown user")
	return fmt.Sprintf("source device %s; Tailscale user %s", device, user)
}

type principalContextKey struct{}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

// WithPrincipal is used by the HTTP auth boundary and by component tests that
// exercise handlers below that already-tested boundary.
func WithPrincipal(r *http.Request, principal Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
}

// Authorize performs a fresh LocalAPI lookup for each request. It returns a
// context copy only on success so a denied request cannot accidentally be
// forwarded with a partial principal.
func (a *Authorizer) Authorize(r *http.Request, required Permission) (*http.Request, Decision) {
	wanted, ok := a.capabilityFor(required)
	if !ok {
		err := fmt.Errorf("unknown operator permission %d", required)
		return nil, deny(http.StatusServiceUnavailable, AuthConfigurationInvalid,
			"operator route does not have a valid capability classification", Principal{}, err)
	}
	if r == nil || strings.TrimSpace(r.RemoteAddr) == "" {
		return nil, deny(http.StatusUnauthorized, Unauthenticated,
			"unable to obtain operator source address from connection", Principal{}, nil)
	}
	if host, ok := remoteHost(r.RemoteAddr); !ok || !tsaddr.IsTailscaleIP(host) {
		return nil, deny(http.StatusUnauthorized, Unauthenticated,
			"operator request must enter directly from a source address identifiable by Tailscale", Principal{SourceAddr: r.RemoteAddr}, nil)
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.timeout)
	defer cancel()
	status, err := a.resolver.Status(ctx)
	if err != nil {
		return nil, deny(http.StatusServiceUnavailable, AuthSourceUnavailable,
			"Tailscale LocalAPI cannot verify daemon and local listener status", Principal{SourceAddr: sourceHost(r.RemoteAddr)}, err)
	}
	if status.BackendState != "Running" {
		err := fmt.Errorf("tailscaled backend state is %q, want Running", status.BackendState)
		return nil, deny(http.StatusServiceUnavailable, AuthSourceUnavailable,
			"tailscaled has not entered Running state, cannot provide operator authority", Principal{SourceAddr: sourceHost(r.RemoteAddr)}, err)
	}
	if !version.AtLeast(status.Version, minimumDaemonVersion) {
		err := fmt.Errorf("tailscaled version %q is older than required %s", status.Version, minimumDaemonVersion)
		return nil, deny(http.StatusServiceUnavailable, AuthSourceUnavailable,
			fmt.Sprintf("tailscaled must be at least %s; otherwise dst_ip cannot serve as authorization boundary", minimumDaemonVersion), Principal{SourceAddr: sourceHost(r.RemoteAddr)}, err)
	}
	if !containsLocalIP(status.TailscaleIPs, a.destination) {
		err := fmt.Errorf("configured destination %s is not one of LocalAPI status IPs %v", a.destination, status.TailscaleIPs)
		return nil, deny(http.StatusServiceUnavailable, AuthConfigurationInvalid,
			"operator destination is not a Tailscale IP currently held by this machine", Principal{SourceAddr: sourceHost(r.RemoteAddr)}, err)
	}
	who, err := a.resolver.WhoIsForIP(ctx, r.RemoteAddr, a.destination)
	if err != nil {
		if errors.Is(err, local.ErrPeerNotFound) {
			return nil, deny(http.StatusUnauthorized, Unauthenticated,
				"Tailscale cannot find identity for this source connection", Principal{SourceAddr: sourceHost(r.RemoteAddr)}, err)
		}
		return nil, deny(http.StatusServiceUnavailable, AuthSourceUnavailable,
			"Tailscale LocalAPI cannot verify operator identity", Principal{SourceAddr: sourceHost(r.RemoteAddr)}, err)
	}
	if who == nil || who.Node == nil {
		err := errors.New("Tailscale LocalAPI returned an incomplete WhoIs response")
		return nil, deny(http.StatusServiceUnavailable, AuthSourceUnavailable,
			"Tailscale LocalAPI returned incomplete identity data", Principal{SourceAddr: sourceHost(r.RemoteAddr)}, err)
	}

	principal := principalFromWho(r.RemoteAddr, who)
	principal.AuthMethod = AuthMethodLocalAPI
	if len(who.Node.Tags) != 0 || who.UserProfile == nil || strings.TrimSpace(who.UserProfile.LoginName) == "" {
		return nil, deny(http.StatusForbidden, HumanPrincipalRequired,
			"operator plane accepts only non-tagged devices with explicit Tailscale users", principal, nil)
	}
	if who.UserProfile.ID == 0 || who.Node.StableID == "" {
		err := errors.New("Tailscale LocalAPI identity lacks a stable user or node ID")
		return nil, deny(http.StatusServiceUnavailable, AuthSourceUnavailable,
			"operator identity returned by Tailscale LocalAPI lacks stable identifier", principal, err)
	}
	granted, err := a.readCapabilities(who.CapMap)
	if err != nil {
		return nil, deny(http.StatusServiceUnavailable, AuthConfigurationInvalid,
			"clawctl capability format in Tailscale grant is invalid", principal, err)
	}
	principal.GrantedCapabilities = granted
	if !principal.Has(required) {
		return nil, deny(http.StatusForbidden, CapabilityRequired,
			fmt.Sprintf("Tailscale identity is missing %s", wanted), principal, nil)
	}

	principal.AuthorizedCapability = wanted
	return WithPrincipal(r, principal), Decision{
		Allowed: true, HTTPStatus: http.StatusOK, Code: Authorized,
		Detail: "Tailscale LocalAPI identity and app capability verified", Principal: principal,
	}
}

func containsLocalIP(local []netip.Addr, destination netip.Addr) bool {
	destination = destination.Unmap()
	for _, candidate := range local {
		if candidate.IsValid() && candidate.Zone() == "" && candidate.Unmap() == destination {
			return true
		}
	}
	return false
}

func (a *Authorizer) capabilityFor(permission Permission) (string, bool) {
	switch permission {
	case View:
		return a.names.View, true
	case Operate:
		return a.names.Operate, true
	case Admin:
		return a.names.Admin, true
	default:
		return "", false
	}
}

func (a *Authorizer) readCapabilities(capMap tailcfg.PeerCapMap) (CapabilityNames, error) {
	var granted CapabilityNames
	checks := []struct {
		name string
		set  *string
	}{
		{name: a.names.View, set: &granted.View},
		{name: a.names.Operate, set: &granted.Operate},
		{name: a.names.Admin, set: &granted.Admin},
	}
	for _, check := range checks {
		values, present := capMap[tailcfg.PeerCapability(check.name)]
		if !present {
			continue
		}
		for _, raw := range values {
			var parameters map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &parameters); err != nil || parameters == nil || len(parameters) != 0 {
				if err == nil {
					err = errors.New("capability parameters must be an empty JSON object")
				}
				return CapabilityNames{}, fmt.Errorf("capability %s: %w", check.name, err)
			}
		}
		*check.set = check.name
	}
	return granted, nil
}

func principalFromWho(remoteAddr string, who *apitype.WhoIsResponse) Principal {
	p := Principal{
		SourceAddr:   sourceHost(remoteAddr),
		NodeStableID: string(who.Node.StableID),
		DeviceName:   who.Node.Name,
	}
	if who.UserProfile != nil {
		p.TailnetUserID = strconv.FormatInt(int64(who.UserProfile.ID), 10)
		p.TailnetUserLogin = strings.TrimSpace(who.UserProfile.LoginName)
		p.TailnetDisplayName = strings.TrimSpace(who.UserProfile.DisplayName)
	}
	return p
}

func deny(status int, code DecisionCode, detail string, principal Principal, cause error) Decision {
	return Decision{HTTPStatus: status, Code: code, Detail: detail, Principal: principal, cause: cause}
}

func remoteHost(remoteAddr string) (netip.Addr, bool) {
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	if addr, err := netip.ParseAddr(remoteAddr); err == nil {
		return addr.Unmap(), true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	return addr.Unmap(), err == nil
}

func sourceHost(remoteAddr string) string {
	if host, ok := remoteHost(remoteAddr); ok {
		return host.String()
	}
	return remoteAddr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
