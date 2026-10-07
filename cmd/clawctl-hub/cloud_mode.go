package main

import (
	"fmt"
	"net/http"
	"os"

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
	if _, err := operatorendpoint.ParseCloudListen(listen); err != nil {
		return cloudBoundaryConfig{}, err
	}
	public, err := operatorendpoint.ParsePublicURL(os.Getenv("CLAWCTL_PUBLIC_URL"))
	if err != nil {
		return cloudBoundaryConfig{}, err
	}
	config := cloudBoundaryConfig{public: public}
	if mode == authModeBoth {
		if endpoint, err := operatorendpoint.ParseListen(listen); err == nil {
			config.tailnetAuthority = endpoint.Authority()
		}
	}
	return config, nil
}
