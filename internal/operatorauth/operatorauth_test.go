package operatorauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

const testCapabilityPrefix = "example.com/cap/clawctl"

type resolverCall struct {
	remoteAddr  string
	destination netip.Addr
}

type fakeResolver struct {
	mu           sync.Mutex
	result       *apitype.WhoIsResponse
	err          error
	resolve      func(context.Context, string, netip.Addr) (*apitype.WhoIsResponse, error)
	calls        []resolverCall
	version      string
	versionErr   error
	versionCalls int
	backendState string
	localIPs     []netip.Addr
}

func (f *fakeResolver) Status(context.Context) (ResolverStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versionCalls++
	if f.versionErr != nil {
		return ResolverStatus{}, f.versionErr
	}
	version := f.version
	if version == "" {
		version = "1.102.2"
	}
	state := f.backendState
	if state == "" {
		state = "Running"
	}
	localIPs := f.localIPs
	if localIPs == nil {
		localIPs = []netip.Addr{netip.MustParseAddr("100.64.200.2")}
	}
	return ResolverStatus{Version: version, BackendState: state, TailscaleIPs: localIPs}, nil
}

func (f *fakeResolver) WhoIsForIP(ctx context.Context, remoteAddr string, destination netip.Addr) (*apitype.WhoIsResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, resolverCall{remoteAddr: remoteAddr, destination: destination})
	resolve := f.resolve
	result, err := f.result, f.err
	f.mu.Unlock()
	if resolve != nil {
		return resolve(ctx, remoteAddr, destination)
	}
	return result, err
}

func (f *fakeResolver) snapshotCalls() []resolverCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]resolverCall(nil), f.calls...)
}

func TestCapabilityNamesAreFixedKeys(t *testing.T) {
	names, err := NamesForPrefix(testCapabilityPrefix)
	if err != nil {
		t.Fatal(err)
	}
	want := CapabilityNames{
		View:    testCapabilityPrefix + "-view",
		Operate: testCapabilityPrefix + "-operate",
		Admin:   testCapabilityPrefix + "-admin",
	}
	if names != want {
		t.Fatalf("NamesForPrefix() = %+v, want %+v", names, want)
	}
	for _, name := range []string{names.View, names.Operate, names.Admin} {
		if strings.Contains(name, "role") || strings.ContainsAny(name, "{}[]?") {
			t.Errorf("capability %q looks parameterized; roles must be fixed keys", name)
		}
	}
}

func TestNewWithResolverRejectsInvalidConfiguration(t *testing.T) {
	validDestination := netip.MustParseAddr("100.64.200.2")
	tests := []struct {
		name     string
		config   Config
		resolver Resolver
	}{
		{name: "nil resolver", config: Config{Destination: validDestination, CapabilityPrefix: testCapabilityPrefix}},
		{name: "invalid destination", config: Config{Destination: netip.Addr{}, CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "IPv4 unspecified", config: Config{Destination: netip.MustParseAddr("0.0.0.0"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "IPv6 unspecified", config: Config{Destination: netip.MustParseAddr("::"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "IPv4 loopback", config: Config{Destination: netip.MustParseAddr("127.0.0.1"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "mapped IPv4 loopback", config: Config{Destination: netip.MustParseAddr("::ffff:127.0.0.1"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "IPv6 loopback", config: Config{Destination: netip.MustParseAddr("::1"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "zoned destination", config: Config{Destination: netip.MustParseAddr("fe80::1%eth0"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "LAN destination", config: Config{Destination: netip.MustParseAddr("192.168.1.20"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "public destination", config: Config{Destination: netip.MustParseAddr("203.0.113.20"), CapabilityPrefix: testCapabilityPrefix}, resolver: &fakeResolver{}},
		{name: "negative timeout", config: Config{Destination: validDestination, CapabilityPrefix: testCapabilityPrefix, ResolveTimeout: -time.Second}, resolver: &fakeResolver{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWithResolver(test.config, test.resolver); err == nil {
				t.Fatal("NewWithResolver() accepted unsafe configuration")
			}
		})
	}
}

func TestAuthorizeAcceptsOnlyParameterFreeExactCapabilities(t *testing.T) {
	names, err := NamesForPrefix(testCapabilityPrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		values     []tailcfg.RawMessage
		wantAllow  bool
		wantStatus int
		wantCode   DecisionCode
	}{
		{name: "no values", values: nil, wantAllow: true, wantStatus: http.StatusOK, wantCode: Authorized},
		{name: "empty object", values: []tailcfg.RawMessage{`{}`}, wantAllow: true, wantStatus: http.StatusOK, wantCode: Authorized},
		{name: "nonempty parameters", values: []tailcfg.RawMessage{`{"role":"admin"}`}, wantStatus: http.StatusServiceUnavailable, wantCode: AuthConfigurationInvalid},
		{name: "null value", values: []tailcfg.RawMessage{`null`}, wantStatus: http.StatusServiceUnavailable, wantCode: AuthConfigurationInvalid},
		{name: "malformed value", values: []tailcfg.RawMessage{`{"broken"`}, wantStatus: http.StatusServiceUnavailable, wantCode: AuthConfigurationInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := humanResponse()
			response.CapMap[tailcfg.PeerCapability(names.View)] = test.values
			authorizer, err := NewWithResolver(Config{
				Destination: netip.MustParseAddr("100.64.200.2"), CapabilityPrefix: testCapabilityPrefix,
			}, &fakeResolver{result: response})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
			req.RemoteAddr = "100.100.10.20:4321"
			authed, decision := authorizer.Authorize(req, View)
			if decision.Allowed != test.wantAllow || decision.HTTPStatus != test.wantStatus || decision.Code != test.wantCode {
				t.Fatalf("decision=%+v", decision)
			}
			if test.wantAllow {
				principal, ok := PrincipalFromContext(authed.Context())
				if !ok || principal.AuthorizedCapability != names.View || principal.AuthMethod != AuthMethodLocalAPI {
					t.Fatalf("principal=%+v ok=%t", principal, ok)
				}
			}
		})
	}
}

func TestAuthorizeRejectsNonTailnetSourcesBeforeLookup(t *testing.T) {
	for _, remoteAddr := range []string{
		"127.0.0.1:1234", "[::1]:1234", "192.168.1.8:1234", "203.0.113.8:1234", "not-an-address",
	} {
		t.Run(remoteAddr, func(t *testing.T) {
			resolver := &fakeResolver{result: humanResponse(testCapabilityPrefix + "-view")}
			authorizer, err := NewWithResolver(Config{
				Destination: netip.MustParseAddr("100.64.200.2"), CapabilityPrefix: testCapabilityPrefix,
			}, resolver)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
			req.RemoteAddr = remoteAddr
			got, decision := authorizer.Authorize(req, View)
			if got != nil || decision.HTTPStatus != http.StatusUnauthorized || decision.Code != Unauthenticated {
				t.Fatalf("request=%v decision=%+v", got, decision)
			}
			if calls := resolver.snapshotCalls(); len(calls) != 0 {
				t.Fatalf("untrusted source reached resolver: %+v", calls)
			}
		})
	}
}

func TestNamesForPrefixRejectsInvalidNamespace(t *testing.T) {
	tests := []string{
		"",
		" example.com/cap/clawctl",
		"example.com/cap/clawctl ",
		"https://example.com/cap/clawctl",
		"EXAMPLE.com/cap/clawctl",
		"localhost/cap/clawctl",
		"example..com/cap/clawctl",
		"-example.com/cap/clawctl",
		"example.com./cap/clawctl",
		"tailscale.com/cap/clawctl",
		"tools.tailscale.io/cap/clawctl",
		"example.com/clawctl",
		"example.com/cap/",
		"example.com/cap/-clawctl",
		"example.com/cap/clawctl-",
		"example.com/cap/claw_ctl",
		"example.com/cap/clawctl/view",
		"example.com/cap/clawctl?role=admin",
	}
	for _, prefix := range tests {
		t.Run(prefix, func(t *testing.T) {
			if _, err := NamesForPrefix(prefix); err == nil {
				t.Fatalf("NamesForPrefix(%q) succeeded", prefix)
			}
		})
	}
}

func TestAuthorizeDecisions(t *testing.T) {
	destination := netip.MustParseAddr("100.64.200.2")
	names, err := NamesForPrefix(testCapabilityPrefix)
	if err != nil {
		t.Fatal(err)
	}
	userNode := &tailcfg.Node{
		StableID: "node-stable-1",
		Name:     "operator-laptop.example.ts.net.",
	}
	user := &tailcfg.UserProfile{
		ID:          42,
		LoginName:   "operator@example.com",
		DisplayName: "Operator Example",
	}
	response := func(caps ...string) *apitype.WhoIsResponse {
		capMap := make(tailcfg.PeerCapMap, len(caps))
		for _, capability := range caps {
			capMap[tailcfg.PeerCapability(capability)] = nil
		}
		return &apitype.WhoIsResponse{Node: userNode, UserProfile: user, CapMap: capMap}
	}

	tests := []struct {
		name       string
		permission Permission
		result     *apitype.WhoIsResponse
		err        error
		wantAllow  bool
		wantStatus int
		wantCode   DecisionCode
		wantCalls  int
	}{
		{name: "exact view", permission: View, result: response(names.View), wantAllow: true, wantStatus: http.StatusOK, wantCode: Authorized, wantCalls: 1},
		{name: "exact operate", permission: Operate, result: response(names.Operate), wantAllow: true, wantStatus: http.StatusOK, wantCode: Authorized, wantCalls: 1},
		{name: "exact admin", permission: Admin, result: response(names.Admin), wantAllow: true, wantStatus: http.StatusOK, wantCode: Authorized, wantCalls: 1},
		{name: "peer absent", permission: View, err: local.ErrPeerNotFound, wantStatus: http.StatusUnauthorized, wantCode: Unauthenticated, wantCalls: 1},
		{name: "wrapped peer absent", permission: View, err: errors.Join(errors.New("lookup"), local.ErrPeerNotFound), wantStatus: http.StatusUnauthorized, wantCode: Unauthenticated, wantCalls: 1},
		{name: "tagged node", permission: View, result: &apitype.WhoIsResponse{Node: &tailcfg.Node{Name: "automation.", Tags: []string{"tag:agent"}}, UserProfile: user, CapMap: response(names.View).CapMap}, wantStatus: http.StatusForbidden, wantCode: HumanPrincipalRequired, wantCalls: 1},
		{name: "missing user profile", permission: View, result: &apitype.WhoIsResponse{Node: userNode, CapMap: response(names.View).CapMap}, wantStatus: http.StatusForbidden, wantCode: HumanPrincipalRequired, wantCalls: 1},
		{name: "missing stable user ID", permission: View, result: &apitype.WhoIsResponse{Node: userNode, UserProfile: &tailcfg.UserProfile{LoginName: "operator@example.com"}, CapMap: response(names.View).CapMap}, wantStatus: http.StatusServiceUnavailable, wantCode: AuthSourceUnavailable, wantCalls: 1},
		{name: "missing stable node ID", permission: View, result: &apitype.WhoIsResponse{Node: &tailcfg.Node{Name: "operator-laptop.example.ts.net."}, UserProfile: user, CapMap: response(names.View).CapMap}, wantStatus: http.StatusServiceUnavailable, wantCode: AuthSourceUnavailable, wantCalls: 1},
		{name: "missing capability", permission: View, result: response(), wantStatus: http.StatusForbidden, wantCode: CapabilityRequired, wantCalls: 1},
		{name: "view does not imply operate", permission: Operate, result: response(names.View), wantStatus: http.StatusForbidden, wantCode: CapabilityRequired, wantCalls: 1},
		{name: "operate does not imply view", permission: View, result: response(names.Operate), wantStatus: http.StatusForbidden, wantCode: CapabilityRequired, wantCalls: 1},
		{name: "admin does not imply operate", permission: Operate, result: response(names.Admin), wantStatus: http.StatusForbidden, wantCode: CapabilityRequired, wantCalls: 1},
		{name: "parameterized generic role is ignored", permission: Admin, result: &apitype.WhoIsResponse{Node: userNode, UserProfile: user, CapMap: tailcfg.PeerCapMap{tailcfg.PeerCapability(testCapabilityPrefix): {tailcfg.RawMessage(`{"role":"admin"}`)}}}, wantStatus: http.StatusForbidden, wantCode: CapabilityRequired, wantCalls: 1},
		{name: "local API error", permission: View, err: errors.New("tailscaled socket unavailable"), wantStatus: http.StatusServiceUnavailable, wantCode: AuthSourceUnavailable, wantCalls: 1},
		{name: "deadline error", permission: View, err: context.DeadlineExceeded, wantStatus: http.StatusServiceUnavailable, wantCode: AuthSourceUnavailable, wantCalls: 1},
		{name: "nil response", permission: View, wantStatus: http.StatusServiceUnavailable, wantCode: AuthSourceUnavailable, wantCalls: 1},
		{name: "nil node", permission: View, result: &apitype.WhoIsResponse{UserProfile: user, CapMap: response(names.View).CapMap}, wantStatus: http.StatusServiceUnavailable, wantCode: AuthSourceUnavailable, wantCalls: 1},
		{name: "unknown permission", permission: Permission(99), result: response(names.View, names.Operate, names.Admin), wantStatus: http.StatusServiceUnavailable, wantCode: AuthConfigurationInvalid, wantCalls: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{result: test.result, err: test.err}
			authorizer, err := NewWithResolver(Config{
				Destination:      destination,
				CapabilityPrefix: testCapabilityPrefix,
			}, resolver)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
			req.RemoteAddr = "100.100.10.20:4321"

			authedRequest, decision := authorizer.Authorize(req, test.permission)
			if decision.Allowed != test.wantAllow || decision.HTTPStatus != test.wantStatus || decision.Code != test.wantCode {
				t.Fatalf("decision = %+v, want allowed=%t status=%d code=%q", decision, test.wantAllow, test.wantStatus, test.wantCode)
			}
			if got := len(resolver.snapshotCalls()); got != test.wantCalls {
				t.Fatalf("resolver calls = %d, want %d", got, test.wantCalls)
			}
			if test.wantAllow {
				if authedRequest == nil {
					t.Fatal("allowed decision returned nil request")
				}
				principal, ok := PrincipalFromContext(authedRequest.Context())
				if !ok {
					t.Fatal("allowed request has no principal in context")
				}
				if principal.DeviceName != "operator-laptop.example.ts.net." || principal.TailnetUserLogin != "operator@example.com" {
					t.Fatalf("principal = %+v", principal)
				}
			} else if authedRequest != nil {
				t.Fatal("denied decision returned a request that could be forwarded")
			}
		})
	}
}

func TestAuthorizeUsesOnlyRemoteAddrAndConfiguredDestination(t *testing.T) {
	destination := netip.MustParseAddr("100.64.200.2")
	names, err := NamesForPrefix(testCapabilityPrefix)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{result: humanResponse(names.View)}
	authorizer, err := NewWithResolver(Config{Destination: destination, CapabilityPrefix: testCapabilityPrefix}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://attacker-controlled-host.example/", nil)
	req.RemoteAddr = "[fd7a:115c:a1e0::1234]:4567"
	spoofed := map[string]string{
		"Forwarded":                  "for=100.64.0.1;host=100.64.0.2",
		"X-Forwarded-For":            "100.64.0.3",
		"X-Real-IP":                  "100.64.0.4",
		"X-Forwarded-User":           "admin@example.com",
		"Tailscale-User-Login":       "admin@example.com",
		"Tailscale-User-Name":        "Administrator",
		"Tailscale-App-Capabilities": `{"example.com/cap/clawctl-admin":[{}]}`,
		"Authorization":              "Bearer not-a-human-credential",
	}
	for name, value := range spoofed {
		req.Header.Set(name, value)
	}

	authedRequest, decision := authorizer.Authorize(req, View)
	if !decision.Allowed || authedRequest == nil {
		t.Fatalf("decision = %+v", decision)
	}
	calls := resolver.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(calls))
	}
	if calls[0].remoteAddr != req.RemoteAddr {
		t.Fatalf("resolver remoteAddr = %q, want Request.RemoteAddr %q", calls[0].remoteAddr, req.RemoteAddr)
	}
	if calls[0].destination != destination {
		t.Fatalf("resolver destination = %s, want trusted configured %s", calls[0].destination, destination)
	}
	principal, ok := PrincipalFromContext(authedRequest.Context())
	if !ok {
		t.Fatal("principal missing")
	}
	if principal.TailnetUserLogin == "admin@example.com" || strings.Contains(principal.Attribution(), "Administrator") {
		t.Fatalf("spoofed header reached principal: %+v / %q", principal, principal.Attribution())
	}
}

func TestSpoofedHeadersCannotReplaceMissingPeer(t *testing.T) {
	resolver := &fakeResolver{err: local.ErrPeerNotFound}
	authorizer, err := NewWithResolver(Config{
		Destination:      netip.MustParseAddr("100.64.200.2"),
		CapabilityPrefix: testCapabilityPrefix,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
	req.RemoteAddr = "100.100.20.30:9999"
	req.Header.Set("Tailscale-User-Login", "operator@example.com")
	req.Header.Set("Tailscale-App-Capabilities", `{"example.com/cap/clawctl-view":[{}]}`)
	req.Header.Set("X-Forwarded-For", "100.100.10.20")

	got, decision := authorizer.Authorize(req, View)
	if got != nil || decision.Allowed || decision.HTTPStatus != http.StatusUnauthorized || decision.Code != Unauthenticated {
		t.Fatalf("spoofed headers changed decision: request=%v decision=%+v", got, decision)
	}
}

func TestEmptyRemoteAddrDoesNotFallBackToHeaders(t *testing.T) {
	resolver := &fakeResolver{result: humanResponse(testCapabilityPrefix + "-view")}
	authorizer, err := NewWithResolver(Config{
		Destination:      netip.MustParseAddr("100.64.200.2"),
		CapabilityPrefix: testCapabilityPrefix,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
	req.RemoteAddr = ""
	req.Header.Set("X-Forwarded-For", "100.100.10.20")

	got, decision := authorizer.Authorize(req, View)
	if got != nil || decision.HTTPStatus != http.StatusUnauthorized || decision.Code != Unauthenticated {
		t.Fatalf("decision = %+v", decision)
	}
	if calls := resolver.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("empty RemoteAddr called resolver with %+v", calls)
	}
}

func TestAuthorizeCallsResolverForEveryRequest(t *testing.T) {
	resolver := &fakeResolver{result: humanResponse(testCapabilityPrefix + "-view")}
	authorizer, err := NewWithResolver(Config{
		Destination:      netip.MustParseAddr("100.64.200.2"),
		CapabilityPrefix: testCapabilityPrefix,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
		req.RemoteAddr = "100.100.10.20:4321"
		if _, decision := authorizer.Authorize(req, View); !decision.Allowed {
			t.Fatalf("decision = %+v", decision)
		}
	}
	if got := len(resolver.snapshotCalls()); got != 2 {
		t.Fatalf("resolver calls = %d, want 2 (authorization must not be cached)", got)
	}
	resolver.mu.Lock()
	versionCalls := resolver.versionCalls
	resolver.mu.Unlock()
	if versionCalls != 2 {
		t.Fatalf("daemon version checks = %d, want 2 (a downgraded daemon must fail closed)", versionCalls)
	}
}

func TestAuthorizeRejectsDaemonThatCannotScopeCapabilitiesToDestination(t *testing.T) {
	for _, test := range []struct {
		name       string
		version    string
		versionErr error
	}{
		{name: "old daemon silently ignores dst_ip", version: "1.98.4"},
		{name: "unparseable version", version: "not-a-version"},
		{name: "status unavailable", versionErr: errors.New("localapi status failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{
				version: test.version, versionErr: test.versionErr,
				result: humanResponse(testCapabilityPrefix + "-admin"),
			}
			authorizer, err := NewWithResolver(Config{
				Destination:      netip.MustParseAddr("100.64.200.2"),
				CapabilityPrefix: testCapabilityPrefix,
			}, resolver)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "http://hub.example/v1/operator/test", nil)
			req.RemoteAddr = "100.100.10.20:4321"
			got, decision := authorizer.Authorize(req, Admin)
			if got != nil || decision.Allowed || decision.HTTPStatus != http.StatusServiceUnavailable ||
				decision.Code != AuthSourceUnavailable {
				t.Fatalf("old/unknown daemon did not fail closed: request=%v decision=%+v", got, decision)
			}
			if calls := resolver.snapshotCalls(); len(calls) != 0 {
				t.Fatalf("WhoIsForIP ran before daemon compatibility was proved: %+v", calls)
			}
		})
	}
}

func TestAuthorizeRequiresRunningDaemonAndLocalDestination(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      string
		localIPs   []netip.Addr
		wantCode   DecisionCode
		wantDetail string
	}{
		{
			name: "backend not running", state: "Starting",
			localIPs: []netip.Addr{netip.MustParseAddr("100.64.200.2")},
			wantCode: AuthSourceUnavailable, wantDetail: "Running",
		},
		{
			name: "destination belongs to another peer", state: "Running",
			localIPs: []netip.Addr{netip.MustParseAddr("100.64.200.4")},
			wantCode: AuthConfigurationInvalid, wantDetail: "not a Tailscale IP currently held by this machine",
		},
		{
			name: "status has no local addresses", state: "Running",
			localIPs: []netip.Addr{},
			wantCode: AuthConfigurationInvalid, wantDetail: "not a Tailscale IP currently held by this machine",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{
				backendState: test.state, localIPs: test.localIPs,
				result: humanResponse(testCapabilityPrefix + "-admin"),
			}
			authorizer, err := NewWithResolver(Config{
				Destination:      netip.MustParseAddr("100.64.200.2"),
				CapabilityPrefix: testCapabilityPrefix,
			}, resolver)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
			req.RemoteAddr = "100.100.10.20:4321"
			got, decision := authorizer.Authorize(req, Admin)
			if got != nil || decision.Allowed || decision.HTTPStatus != http.StatusServiceUnavailable ||
				decision.Code != test.wantCode || !strings.Contains(decision.Detail, test.wantDetail) {
				t.Fatalf("request=%v decision=%+v", got, decision)
			}
			if calls := resolver.snapshotCalls(); len(calls) != 0 {
				t.Fatalf("WhoIsForIP ran before local status was proved: %+v", calls)
			}
		})
	}
}

func TestAuthorizeBoundsEveryLookupWithTimeout(t *testing.T) {
	resolver := &fakeResolver{resolve: func(ctx context.Context, _ string, _ netip.Addr) (*apitype.WhoIsResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("resolver context had no deadline")
		}
		if time.Until(deadline) > 100*time.Millisecond {
			return nil, errors.New("resolver deadline exceeded configured bound")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	authorizer, err := NewWithResolver(Config{
		Destination:      netip.MustParseAddr("100.64.200.2"),
		CapabilityPrefix: testCapabilityPrefix,
		ResolveTimeout:   20 * time.Millisecond,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
	req.RemoteAddr = "100.100.10.20:4321"

	got, decision := authorizer.Authorize(req, View)
	if got != nil || decision.HTTPStatus != http.StatusServiceUnavailable || decision.Code != AuthSourceUnavailable {
		t.Fatalf("timeout did not fail closed: request=%v decision=%+v", got, decision)
	}
	if !errors.Is(decision.Cause(), context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want context deadline exceeded", decision.Cause())
	}
}

func TestPrincipalContextAndHonestAttribution(t *testing.T) {
	authorizer, err := NewWithResolver(Config{
		Destination:      netip.MustParseAddr("100.64.200.2"),
		CapabilityPrefix: testCapabilityPrefix,
	}, &fakeResolver{result: humanResponse(testCapabilityPrefix + "-view")})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
	req.RemoteAddr = "100.100.10.20:4321"
	if _, ok := PrincipalFromContext(req.Context()); ok {
		t.Fatal("unauthorized request already had a principal")
	}

	authedRequest, decision := authorizer.Authorize(req, View)
	if !decision.Allowed {
		t.Fatalf("decision = %+v", decision)
	}
	principal, ok := PrincipalFromContext(authedRequest.Context())
	if !ok {
		t.Fatal("authorized request principal missing")
	}
	wording := principal.Attribution()
	for _, want := range []string{
		"source device operator-laptop.example.ts.net.",
		"Tailscale user operator@example.com",
	} {
		if !strings.Contains(wording, want) {
			t.Errorf("Attribution() = %q, missing %q", wording, want)
		}
	}
	if req == authedRequest {
		t.Fatal("Authorize mutated the caller's request instead of returning a context copy")
	}
	if _, ok := PrincipalFromContext(req.Context()); ok {
		t.Fatal("Authorize mutated the original request context")
	}
	if principal.StableSubject() != "tailscale-user:42" {
		t.Fatalf("StableSubject() = %q", principal.StableSubject())
	}
}

// TestAuthorizeRefusesAHumanNodeWithNoLoginName 守住逐臂普查中全樹全綠的三格之一。
// 既有測試從未造過「非 tagged 裝置 + UserProfile 非 nil + LoginName 為空」的組合。
// 不能只靠 HTTP 邊界：operator_boundary.go:613-615 會在 TailnetUserLogin 為空時，
// 改判 503 AUTH_CONFIGURATION_INVALID（「控制面身分驗證回傳不完整」），並清空稽核
// Principal。這雖然 fail-closed，卻不是同一道判決：Authorize 應回 403
// HumanPrincipalRequired（「這台不是人」），邊界則表示「Hub 設定壞了」；oncall 看到的
// 是兩件完全不同的事，所以這一項必須自己守住自己的判決。
// 可達性並非假想：tailcfg.UserProfile.LoginName 是普通 string、沒有 omitempty，註解明寫
// "for display purposes only"；Tailscale 的 ipnlocal 也用 if newLoginName != "" 與
// <missing-profile> 後備。apitype.WhoIsResponse 只保證成功時 Node 與 UserProfile 非 nil，
// 並未保證 LoginName 非空；docs/VERIFIER-TOPOLOGY.md:200 也明文寫著會拒絕沒有
// UserProfile.LoginName 的 node。
// 兩個子測試分別餵空字串與純空白，因 production 用 strings.TrimSpace(...) == ""；
// 若只餵空字串，移除 TrimSpace 後測試仍會通過。ID: 42 刻意避開後續 ID == 0 的拒絕，
// DisplayName 則證明看似真人的名稱不能取代 login name。
func TestAuthorizeRefusesAHumanNodeWithNoLoginName(t *testing.T) {
	tests := []struct {
		name      string
		loginName string
	}{
		{name: "login name 是空字串", loginName: ""},
		{name: "login name 只有空白", loginName: "   "},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := humanResponse(testCapabilityPrefix + "-view")
			response.UserProfile = &tailcfg.UserProfile{
				ID:          42,
				LoginName:   test.loginName,
				DisplayName: "Operator Example",
			}
			authorizer, err := NewWithResolver(Config{
				Destination:      netip.MustParseAddr("100.64.200.2"),
				CapabilityPrefix: testCapabilityPrefix,
			}, &fakeResolver{result: response})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
			req.RemoteAddr = "100.100.10.20:4321"

			got, decision := authorizer.Authorize(req, View)
			if got != nil || decision.Allowed || decision.HTTPStatus != http.StatusForbidden ||
				decision.Code != HumanPrincipalRequired {
				t.Fatalf("node without a login name was not rejected with 403 HumanPrincipalRequired: request=%v decision=%+v", got, decision)
			}
		})
	}
}

// TestAuthorizeDoesNotTreatAZonedMappedAddressAsItsOwnListener 守住逐臂普查中
// 全樹全綠的一項：既有測試沒有餵過帶 zone 的 candidate。這並非冗餘；純 IPv4
// 不能帶 zone，原生 IPv6 帶 zone 時 Unmap 後的比較仍會因 zone 不同而失敗，且
// NewWithResolver 已拒絕帶 zone 的 destination。只有帶 zone 的 4-in-6 會在 Unmap
// 時吃掉 zone，因此是唯一能分辨有無這項檢查的輸入。status.TailscaleIPs 是外部
// daemon 交來的資料，所以這裡驗的是外部邊界，不是對自己程式碼的
// defence-in-depth。無 zone 的反向對照則防止「凡 4-in-6 一律不算」這種會鎖死
// 合法設定的收緊；若只測拒收，該收緊在紅燈盤上完全隱形。
func TestAuthorizeDoesNotTreatAZonedMappedAddressAsItsOwnListener(t *testing.T) {
	t.Run("帶 zone 的 4-in-6 不算這台機器的 listener", func(t *testing.T) {
		resolver := &fakeResolver{
			localIPs: []netip.Addr{netip.MustParseAddr("::ffff:100.64.200.2%eth0")},
			result:   humanResponse(testCapabilityPrefix + "-admin"),
		}
		authorizer, err := NewWithResolver(Config{
			Destination:      netip.MustParseAddr("100.64.200.2"),
			CapabilityPrefix: testCapabilityPrefix,
		}, resolver)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
		req.RemoteAddr = "100.100.10.20:4321"

		got, decision := authorizer.Authorize(req, Admin)
		if got != nil || decision.Allowed || decision.HTTPStatus != http.StatusServiceUnavailable ||
			decision.Code != AuthConfigurationInvalid {
			t.Fatalf("zoned mapped address was accepted as the local listener: request=%v decision=%+v", got, decision)
		}
		if calls := resolver.snapshotCalls(); len(calls) != 0 {
			t.Fatalf("WhoIsForIP ran before the zoned destination candidate was rejected: %+v", calls)
		}
	})

	t.Run("同一個位址不帶 zone 就算", func(t *testing.T) {
		resolver := &fakeResolver{
			localIPs: []netip.Addr{netip.MustParseAddr("::ffff:100.64.200.2")},
			result:   humanResponse(testCapabilityPrefix + "-admin"),
		}
		authorizer, err := NewWithResolver(Config{
			Destination:      netip.MustParseAddr("100.64.200.2"),
			CapabilityPrefix: testCapabilityPrefix,
		}, resolver)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
		req.RemoteAddr = "100.100.10.20:4321"

		_, decision := authorizer.Authorize(req, Admin)
		if !decision.Allowed {
			t.Fatalf("unzoned mapped address was not accepted as the local listener: decision=%+v", decision)
		}
	})
}

// TestAuthorizeDoesNotReportAnUnreachableDaemonAsNotRunning 守住相鄰兩臂的語意：
// 它們共用同一個 503 與 AuthSourceUnavailable，唯一差別是 Detail 句子與 cause。
// 既有的 "status unavailable" 子測試只斷言 status、code 與呼叫數，而 A4 也全都
// 滿足，所以它不是這項差異的看守者。Detail 會出現在 operator 的 HTTP body，也會
// 被拼進 unsafe method 的 audit_log.detail；講錯的是產品文案與稽核紀錄，不是內部
// 細節。因此這裡刻意不重複斷言 status、code 或呼叫數，只守住真實原因與說法。
func TestAuthorizeDoesNotReportAnUnreachableDaemonAsNotRunning(t *testing.T) {
	localAPIDown := errors.New("localapi dial failed")
	unreachableResolver := &fakeResolver{
		versionErr: localAPIDown,
		result:     humanResponse(testCapabilityPrefix + "-admin"),
	}
	unreachableAuthorizer, err := NewWithResolver(Config{
		Destination:      netip.MustParseAddr("100.64.200.2"),
		CapabilityPrefix: testCapabilityPrefix,
	}, unreachableResolver)
	if err != nil {
		t.Fatal(err)
	}
	unreachableRequest := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
	unreachableRequest.RemoteAddr = "100.100.10.20:4321"
	_, unreachable := unreachableAuthorizer.Authorize(unreachableRequest, Admin)

	notRunningResolver := &fakeResolver{
		backendState: "Starting",
		result:       humanResponse(testCapabilityPrefix + "-admin"),
	}
	notRunningAuthorizer, err := NewWithResolver(Config{
		Destination:      netip.MustParseAddr("100.64.200.2"),
		CapabilityPrefix: testCapabilityPrefix,
	}, notRunningResolver)
	if err != nil {
		t.Fatal(err)
	}
	notRunningRequest := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
	notRunningRequest.RemoteAddr = "100.100.10.20:4321"
	_, notRunning := notRunningAuthorizer.Authorize(notRunningRequest, Admin)

	if !errors.Is(unreachable.Cause(), localAPIDown) {
		t.Fatalf("unreachable LocalAPI cause was not preserved, so the Hub process log lost its only diagnostic clue: decision=%+v", unreachable)
	}
	if unreachable.Detail == notRunning.Detail {
		t.Fatalf("an unreachable daemon and a daemon that reported itself not Running used the same detail: unreachable=%q notRunning=%q", unreachable.Detail, notRunning.Detail)
	}
	if strings.Contains(unreachable.Detail, "Running") {
		t.Fatalf("the response claimed a Running state even though the Hub could not query the backend state: decision=%+v", unreachable)
	}
}

// TestEveryOperatorDenialSaysWhichThingToFix 守住每種失敗給 operator 的下一步。
// 把某一臂的 Detail 換成同 status、同 code 的鄰居句子，十三發有十一發全樹全綠。
// Detail 是產品文案：會進 operator 的 HTTP body，也會進 unsafe method 的
// audit_log.detail，所以講錯的是要去修哪一樣東西。這支只釘區別，不釘字面，
// 因此改寫文案不會誤傷。這裡刻意不要求 WhoIs 呼叫失敗、LocalAPI 回傳不完整的
// 身分資料、身分缺少穩定識別碼三句彼此不同；三者下一步都是回報 daemon 或
// tailnet 有問題，將來合併是合理簡化。A3 與 A4（問不到 daemon 與 daemon 說還沒
// Running）已由 TestAuthorizeDoesNotReportAnUnreachableDaemonAsNotRunning 守住，
// 這裡不重複。
func TestEveryOperatorDenialSaysWhichThingToFix(t *testing.T) {
	detailFor := func(t *testing.T, resolver *fakeResolver, remoteAddr string,
		required Permission) string {
		t.Helper()
		authorizer, err := NewWithResolver(Config{
			Destination:      netip.MustParseAddr("100.64.200.2"),
			CapabilityPrefix: testCapabilityPrefix,
		}, resolver)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://hub.example/", nil)
		req.RemoteAddr = remoteAddr
		got, decision := authorizer.Authorize(req, required)
		if got != nil || decision.Allowed {
			t.Fatalf("the denial scenario unexpectedly allowed the request: request=%v decision=%+v", got, decision)
		}
		if strings.TrimSpace(decision.Detail) == "" {
			t.Fatalf("the denial scenario returned no operator guidance: decision=%+v", decision)
		}
		return decision.Detail
	}

	const tailnetSource = "100.100.10.20:4321"
	view := testCapabilityPrefix + "-view"
	offTailnet := detailFor(t, &fakeResolver{result: humanResponse(view)},
		"203.0.113.8:1234", View)
	peerUnknown := detailFor(t, &fakeResolver{err: local.ErrPeerNotFound}, tailnetSource, View)
	destinationNotLocal := detailFor(t, &fakeResolver{
		localIPs: []netip.Addr{netip.MustParseAddr("100.64.200.4")},
		result:   humanResponse(view),
	}, tailnetSource, View)
	malformedResponse := humanResponse()
	malformedResponse.CapMap[tailcfg.PeerCapability(view)] =
		[]tailcfg.RawMessage{`{"broken"`}
	grantMalformed := detailFor(t, &fakeResolver{result: malformedResponse}, tailnetSource, View)
	daemonTooOld := detailFor(t, &fakeResolver{
		version: "1.98.4",
		result:  humanResponse(view),
	}, tailnetSource, View)
	daemonUnreachable := detailFor(t, &fakeResolver{
		versionErr: errors.New("localapi dial failed"),
		result:     humanResponse(view),
	}, tailnetSource, View)
	daemonNotRunning := detailFor(t, &fakeResolver{
		backendState: "Starting",
		result:       humanResponse(view),
	}, tailnetSource, View)
	taggedResponse := humanResponse(view)
	taggedResponse.Node.Tags = []string{"tag:agent"}
	taggedNode := detailFor(t, &fakeResolver{result: taggedResponse}, tailnetSource, View)
	capabilityMissing := detailFor(t, &fakeResolver{result: humanResponse()}, tailnetSource, View)

	tests := []struct {
		name string
		a    string
		b    string
		why  string
	}{
		{
			name: "非 tailnet 來源 vs 查不到身分",
			a:    offTailnet,
			b:    peerUnknown,
			why:  "一個要改連線路徑，一個要讓這個節點重新被 tailnet 認得。",
		},
		{
			name: "Hub 的 destination 設錯 vs tailnet grant 格式錯",
			a:    destinationNotLocal,
			b:    grantMalformed,
			why:  "一個要改 Hub 的啟動參數，一個要改 tailnet ACL。",
		},
		{
			name: "daemon 太舊 vs 問不到 daemon",
			a:    daemonTooOld,
			b:    daemonUnreachable,
			why:  "一個要升級 tailscaled，一個要查 LocalAPI socket 通不通。",
		},
		{
			name: "daemon 太舊 vs daemon 說自己還沒 Running",
			a:    daemonTooOld,
			b:    daemonNotRunning,
			why:  "一個要升級，一個要等它把登入或啟動走完。",
		},
		{
			name: "不是人 vs 是人但缺 capability",
			a:    taggedNode,
			b:    capabilityMissing,
			why:  "一個要換一個真人身分連進來，一個要請管理者補上 grant。",
		},
	}
	for _, test := range tests {
		if test.a == test.b {
			t.Fatalf("two different operator failures share one sentence, so the denial does not say which thing to fix (%s; %s): a=%q b=%q", test.name, test.why, test.a, test.b)
		}
	}
}

func humanResponse(caps ...string) *apitype.WhoIsResponse {
	capMap := make(tailcfg.PeerCapMap, len(caps))
	for _, capability := range caps {
		capMap[tailcfg.PeerCapability(capability)] = nil
	}
	return &apitype.WhoIsResponse{
		Node: &tailcfg.Node{
			StableID: "node-stable-1",
			Name:     "operator-laptop.example.ts.net.",
		},
		UserProfile: &tailcfg.UserProfile{
			ID:          42,
			LoginName:   "operator@example.com",
			DisplayName: "Operator Example",
		},
		CapMap: capMap,
	}
}
