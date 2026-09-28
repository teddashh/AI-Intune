package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

type authCheckFake struct {
	decision operatorauth.Decision
	authed   func(*http.Request, operatorauth.Decision) *http.Request
	remotes  []string
	required []operatorauth.Permission
}

func (f *authCheckFake) Authorize(r *http.Request, required operatorauth.Permission) (*http.Request, operatorauth.Decision) {
	f.remotes = append(f.remotes, r.RemoteAddr)
	f.required = append(f.required, required)
	if f.authed != nil {
		return f.authed(r, f.decision), f.decision
	}
	if f.decision.Allowed {
		return operatorauth.WithPrincipal(r, f.decision.Principal), f.decision
	}
	return nil, f.decision
}

func readyAuthDecision(prefix string) operatorauth.Decision {
	names, err := operatorauth.NamesForPrefix(prefix)
	if err != nil {
		panic(err)
	}
	return operatorauth.Decision{
		Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized,
		Principal: operatorauth.Principal{
			SourceAddr: "100.64.200.2", NodeStableID: "node-stable", DeviceName: "samplehub1.example.ts.net.",
			TailnetUserID: "42", TailnetUserLogin: "operator@example.com",
			AuthMethod:           operatorauth.AuthMethodLocalAPI,
			AuthorizedCapability: names.Admin, GrantedCapabilities: names,
		},
	}
}

func TestOperatorAuthCheckProvesAllExactCapabilitiesWithoutOpeningStore(t *testing.T) {
	const prefix = "example.com/cap/clawctl"
	fake := &authCheckFake{decision: readyAuthDecision(prefix)}
	var gotConfig operatorauth.Config
	var out bytes.Buffer
	err := runOperatorAuthCheckWithFactory([]string{
		"--operator-auth-check", "--listen", "100.64.200.2:8787",
		"--operator-capability-prefix", prefix,
	}, &out, func(config operatorauth.Config) (operatorRequestAuthorizer, error) {
		gotConfig = config
		return fake, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotConfig.Destination != netip.MustParseAddr("100.64.200.2") || gotConfig.CapabilityPrefix != prefix {
		t.Fatalf("factory config = %+v", gotConfig)
	}
	if strings.Join(fake.remotes, ",") != "100.64.200.2:1" {
		t.Fatalf("probe remotes=%q", fake.remotes)
	}
	if len(fake.required) != 1 || fake.required[0] != operatorauth.Admin {
		t.Fatalf("permissions=%v", fake.required)
	}
	for _, want := range []string{"operator-auth-ready:v1", "100.64.200.2", "authority=100.64.200.2:8787", "view,operate,admin", "self-only=true"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %s", want, out.String())
		}
	}
}

func TestOperatorAuthCheckUsesSameSuccessContractAsHTTPBoundary(t *testing.T) {
	const prefix = "example.com/cap/clawctl"
	for _, tc := range []struct {
		name   string
		mutate func(*operatorauth.Decision)
		authed func(*http.Request, operatorauth.Decision) *http.Request
	}{
		{
			name:   "allowed with nil request",
			authed: func(*http.Request, operatorauth.Decision) *http.Request { return nil },
		},
		{
			name:   "missing principal context",
			authed: func(r *http.Request, _ operatorauth.Decision) *http.Request { return r },
		},
		{
			name:   "wrong status",
			mutate: func(d *operatorauth.Decision) { d.HTTPStatus = http.StatusCreated },
		},
		{
			name:   "wrong code",
			mutate: func(d *operatorauth.Decision) { d.Code = operatorauth.CapabilityRequired },
		},
		{
			name:   "wrong auth method",
			mutate: func(d *operatorauth.Decision) { d.Principal.AuthMethod = "header" },
		},
		{
			name: "wrong authorized capability",
			mutate: func(d *operatorauth.Decision) {
				d.Principal.AuthorizedCapability = d.Principal.GrantedCapabilities.View
			},
		},
		{
			name: "adapter mutates route",
			authed: func(r *http.Request, d operatorauth.Decision) *http.Request {
				authed := operatorauth.WithPrincipal(r, d.Principal)
				urlCopy := *authed.URL
				urlCopy.Path = "/different"
				authed.URL = &urlCopy
				return authed
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := readyAuthDecision(prefix)
			if tc.mutate != nil {
				tc.mutate(&decision)
			}
			fake := &authCheckFake{decision: decision, authed: tc.authed}
			err := runOperatorAuthCheckWithFactory([]string{
				"--operator-auth-check", "--listen", "100.64.200.2:8787",
				"--operator-capability-prefix", prefix,
			}, &bytes.Buffer{}, func(operatorauth.Config) (operatorRequestAuthorizer, error) { return fake, nil })
			if err == nil || (!strings.Contains(err.Error(), "success 判決不完整") &&
				!strings.Contains(err.Error(), "不一致的拒絕判決")) {
				t.Fatalf("contradictory success err=%v", err)
			}
		})
	}
}

func TestOperatorAuthCheckFailsClosedForMissingIndependentCapability(t *testing.T) {
	const prefix = "example.com/cap/clawctl"
	for _, permission := range []operatorauth.Permission{operatorauth.View, operatorauth.Operate, operatorauth.Admin} {
		t.Run(permission.String(), func(t *testing.T) {
			decision := readyAuthDecision(prefix)
			switch permission {
			case operatorauth.View:
				decision.Principal.GrantedCapabilities.View = ""
			case operatorauth.Operate:
				decision.Principal.GrantedCapabilities.Operate = ""
			case operatorauth.Admin:
				decision.Principal.GrantedCapabilities.Admin = ""
			}
			fake := &authCheckFake{decision: decision}
			err := runOperatorAuthCheckWithFactory([]string{
				"--operator-auth-check", "--listen", "100.64.200.2:8787",
				"--operator-capability-prefix", prefix,
			}, &bytes.Buffer{}, func(operatorauth.Config) (operatorRequestAuthorizer, error) { return fake, nil })
			if permission == operatorauth.Admin {
				if err == nil || !strings.Contains(err.Error(), "success 判決不完整") {
					t.Fatalf("missing required admin grant err=%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), permission.String()+" capability") ||
				!strings.Contains(err.Error(), "不做程式內權限繼承") {
				t.Fatalf("missing independent capability err=%v", err)
			}
		})
	}
}

func TestOperatorAuthCheckReportsLocalAPIDenial(t *testing.T) {
	fake := &authCheckFake{decision: operatorauth.Decision{
		HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired,
		Detail: "缺 capability",
	}}
	err := runOperatorAuthCheckWithFactory([]string{
		"--operator-auth-check", "--listen", "100.64.200.2:8787",
		"--operator-capability-prefix", "example.com/cap/clawctl",
	}, &bytes.Buffer{}, func(operatorauth.Config) (operatorRequestAuthorizer, error) { return fake, nil })
	if err == nil || !strings.Contains(err.Error(), string(operatorauth.CapabilityRequired)) || !strings.Contains(err.Error(), "缺 capability") {
		t.Fatalf("denial err=%v", err)
	}
}

func TestOperatorAuthCheckRejectsContradictoryDenial(t *testing.T) {
	fake := &authCheckFake{decision: operatorauth.Decision{
		HTTPStatus: http.StatusOK, Code: operatorauth.Authorized,
		Detail: "not actually allowed",
	}}
	err := runOperatorAuthCheckWithFactory([]string{
		"--operator-auth-check", "--listen", "100.64.200.2:8787",
		"--operator-capability-prefix", "example.com/cap/clawctl",
	}, &bytes.Buffer{}, func(operatorauth.Config) (operatorRequestAuthorizer, error) { return fake, nil })
	if err == nil || !strings.Contains(err.Error(), "不一致的拒絕判決") {
		t.Fatalf("contradictory denial err=%v", err)
	}
}

func TestOperatorAuthCheckReadsServiceConfigurationFromEnvironment(t *testing.T) {
	t.Setenv("CLAWCTL_LISTEN", "100.64.200.2:8787")
	t.Setenv("CLAWCTL_OPERATOR_CAPABILITY_PREFIX", "example.com/cap/clawctl")
	fake := &authCheckFake{decision: readyAuthDecision("example.com/cap/clawctl")}
	if err := runOperatorAuthCheckWithFactory([]string{"--operator-auth-check"}, &bytes.Buffer{},
		func(operatorauth.Config) (operatorRequestAuthorizer, error) { return fake, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorAuthCheckSharesServePublicAuthorityValidation(t *testing.T) {
	t.Setenv("CLAWCTL_PUBLIC_URL", "https://different-origin.example")
	called := false
	err := runOperatorAuthCheckWithFactory([]string{
		"--operator-auth-check", "--listen", "100.64.200.2:8787",
		"--operator-capability-prefix", "example.com/cap/clawctl",
	}, &bytes.Buffer{}, func(operatorauth.Config) (operatorRequestAuthorizer, error) {
		called = true
		return nil, errors.New("must not be reached")
	})
	if err == nil || !strings.Contains(err.Error(), "CLAWCTL_PUBLIC_URL") || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("public authority conflict err=%v", err)
	}
	if called {
		t.Fatal("public authority conflict reached LocalAPI factory")
	}
}

func TestOperatorAuthCheckRejectsInputsBeforeResolver(t *testing.T) {
	called := false
	factory := func(operatorauth.Config) (operatorRequestAuthorizer, error) {
		called = true
		return nil, errors.New("must not be reached")
	}
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{name: "loopback listener", argv: []string{"--operator-auth-check", "--listen", "127.0.0.1:8787", "--operator-capability-prefix", "example.com/cap/clawctl"}},
		{name: "extra positional", argv: []string{"--operator-auth-check", "--listen", "100.64.200.2:8787", "--operator-capability-prefix", "example.com/cap/clawctl", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			if err := runOperatorAuthCheckWithFactory(tc.argv, &bytes.Buffer{}, factory); err == nil {
				t.Fatal("unsafe preflight input was accepted")
			}
			if called {
				t.Fatal("unsafe input reached LocalAPI factory")
			}
		})
	}
}
