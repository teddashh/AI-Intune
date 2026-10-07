package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
)

type authMode string

const (
	authModeTailscale authMode = "tailscale"
	authModeLocal     authMode = "local"
	authModeBoth      authMode = "both"
)

func parseAuthMode(raw string) (authMode, error) {
	switch authMode(raw) {
	case "", authModeTailscale:
		return authModeTailscale, nil
	case authModeLocal, authModeBoth:
		return authMode(raw), nil
	default:
		return "", fmt.Errorf("CLAWCTL_AUTH_MODE/--auth-mode must be tailscale, local, or both")
	}
}

// cloudBoundaryConfig contains only startup-validated origins, never proxy headers.
type cloudBoundaryConfig struct {
	requireMFA       bool
	setupCode        *setupCodeHash
	mode             authMode
	public           operatorendpoint.PublicURL
	tailnetAuthority string
}

func (c cloudBoundaryConfig) matches(host string) bool {
	if c.public.MatchesAuthority(host) {
		return true
	}
	canonical, ok := canonicalLiteralAuthority(host)
	return c.tailnetAuthority != "" && ok && canonical == c.tailnetAuthority
}

type denyOperatorAuthorizer struct{}

func (denyOperatorAuthorizer) Authorize(*http.Request, operatorauth.Permission) (*http.Request, operatorauth.Decision) {
	return nil, operatorauth.Decision{HTTPStatus: http.StatusUnauthorized, Code: operatorauth.Unauthenticated, Detail: "Operator authentication required"}
}

func configuredOperatorAuthorizer(mode authMode, listen, prefix string) (operatorRequestAuthorizer, error) {
	if mode == authModeLocal {
		return denyOperatorAuthorizer{}, nil
	}
	endpoint, err := operatorendpoint.ParseListen(listen)
	if err != nil {
		if mode == authModeBoth {
			return denyOperatorAuthorizer{}, nil
		}
		return nil, err
	}
	return operatorauth.New(operatorauth.Config{Destination: endpoint.Destination(), CapabilityPrefix: prefix})
}

func cloudConfiguration(mode authMode, listen string) (cloudBoundaryConfig, error) {
	endpoint, err := operatorendpoint.ParseCloudListen(listen)
	if err != nil {
		return cloudBoundaryConfig{}, err
	}
	public, err := operatorendpoint.ParsePublicURL(os.Getenv("CLAWCTL_PUBLIC_URL"))
	if err != nil {
		return cloudBoundaryConfig{}, err
	}
	if public.Scheme() == "http" && !endpoint.Destination().IsLoopback() {
		return cloudBoundaryConfig{}, fmt.Errorf("HTTP CLAWCTL_PUBLIC_URL requires a loopback CLAWCTL_LISTEN/--listen IP (127.0.0.0/8 or ::1); use HTTPS for non-loopback listeners")
	}
	required, err := mfaEnforcement(mode, nil)
	if err != nil {
		return cloudBoundaryConfig{}, err
	}
	config := cloudBoundaryConfig{public: public, mode: mode, requireMFA: required}
	if mode == authModeBoth {
		if endpoint, err := operatorendpoint.ParseListen(listen); err == nil {
			config.tailnetAuthority = endpoint.Authority()
		}
	}
	return config, nil
}

const mfaDisabledWarning = "MFA enforcement disabled by CLAWCTL_REQUIRE_MFA=0; local admin accounts can sign in with a password only"

func mfaEnforcement(mode authMode, printf func(string, ...any)) (bool, error) {
	if mode == authModeTailscale {
		return false, nil
	}
	raw, set := os.LookupEnv("CLAWCTL_REQUIRE_MFA")
	if !set {
		return true, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "on":
		return true, nil
	case "0", "false", "off":
		if printf != nil {
			printf("WARNING: %s", mfaDisabledWarning)
		}
		return false, nil
	default:
		return false, fmt.Errorf("CLAWCTL_REQUIRE_MFA must be 0/false/off or 1/true/on")
	}
}
