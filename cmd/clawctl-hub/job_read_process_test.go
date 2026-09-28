package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

func runJobReadTestCLI() error {
	var cfg struct {
		URL  string   `json:"url"`
		Args []string `json:"args"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		return err
	}
	if len(cfg.Args) == 0 || (cfg.Args[0] != "show" && cfg.Args[0] != "evidence") {
		return fmt.Errorf("args must start with show or evidence")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return err
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Port() == "" || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("url must be http://<loopback-ip>:<port>")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	var d net.Dialer
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != testOperatorAuthority {
			return nil, fmt.Errorf("refusing to dial %s", addr)
		}
		return d.DialContext(ctx, network, u.Host)
	}
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	expectedBase := "http://" + testOperatorAuthority
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			return expectedBase, nil
		},
		newOperatorClient: func(base string) (*operatorclient.Client, error) {
			if base != expectedBase {
				return nil, fmt.Errorf("unexpected operator base %q", base)
			}
			return operatorclient.NewWithHTTPClient(base, httpClient)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return runJobReadCommandWithDeps(ctx, cfg.Args, os.Stdout, os.Stderr, deps)
}
