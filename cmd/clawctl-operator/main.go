// clawctl-operator is the thin operator CLI and stdio MCP server.
// It calls the Hub JSON API. It does not open the ledger.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operatoragent"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(errOut, usageText)
		return errUsage
	}
	if args[0] == "version" {
		fmt.Fprintln(out, version)
		return nil
	}
	hubURL, rest, err := hubURLFromArgs(args)
	if err != nil {
		fmt.Fprintln(errOut, err.Error())
		return err
	}
	if len(rest) == 0 {
		fmt.Fprint(errOut, usageText)
		return errUsage
	}
	client, err := operatorclient.New(hubURL)
	if err != nil {
		fmt.Fprintln(errOut, err.Error())
		return err
	}
	svc := &operatoragent.Service{Hub: client, Version: version}
	switch rest[0] {
	case "mcp":
		if len(rest) != 1 {
			fmt.Fprintln(errOut, "mcp does not take arguments")
			return errUsage
		}
		if err := operatoragent.Serve(context.Background(), in, out, svc); err != nil {
			fmt.Fprintln(errOut, err.Error())
			return err
		}
		return nil
	case "call":
		if len(rest) != 2 && len(rest) != 3 {
			fmt.Fprintln(errOut, "usage: clawctl-operator call <tool> [json]")
			return errUsage
		}
		var raw json.RawMessage
		if len(rest) == 3 {
			raw = json.RawMessage(rest[2])
		}
		result, callErr := svc.Call(context.Background(), rest[1], raw)
		if callErr != nil {
			encoded, _ := json.Marshal(callErrorPayload(callErr))
			fmt.Fprintln(errOut, string(encoded))
			return callErr
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintln(errOut, err.Error())
			return err
		}
		fmt.Fprintln(out, string(encoded))
		return nil
	case "tools":
		encoded, err := json.Marshal(operatoragent.Tools())
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(encoded))
		return nil
	default:
		fmt.Fprint(errOut, usageText)
		return errUsage
	}
}

func callErrorPayload(err error) any {
	var call *operatoragent.CallError
	if errors.As(err, &call) {
		return call
	}
	return map[string]string{"code": "hub_error", "message": err.Error()}
}

func hubURLFromArgs(args []string) (string, []string, error) {
	selected := ""
	explicit := false
	i := 0
	for i < len(args) {
		switch {
		case args[i] == "--hub-url":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", nil, errors.New("--hub-url requires a value")
			}
			selected = args[i+1]
			explicit = true
			i += 2
		case strings.HasPrefix(args[i], "--hub-url="):
			selected = strings.TrimPrefix(args[i], "--hub-url=")
			explicit = true
			i++
		default:
			goto done
		}
	}
done:
	if !explicit {
		raw, present := os.LookupEnv("CLAWCTL_HUB_URL")
		if !present || raw == "" || strings.TrimSpace(raw) != raw {
			if present {
				return "", nil, errors.New("CLAWCTL_HUB_URL is set but empty or padded")
			}
			return "", nil, errors.New("set --hub-url or CLAWCTL_HUB_URL to the Hub Tailscale origin")
		}
		selected = raw
	}
	if selected == "" || strings.TrimSpace(selected) != selected {
		return "", nil, errors.New("--hub-url is empty or padded")
	}
	endpoint, err := operatorendpoint.ParseBaseURL(selected)
	if err != nil {
		return "", nil, fmt.Errorf("operator Hub URL: %w", err)
	}
	return endpoint.BaseURL(), args[i:], nil
}

const usageText = `clawctl-operator — call the Hub operator JSON API from a tailnet node

  clawctl-operator [--hub-url http://100.x.y.z:8787] mcp
  clawctl-operator [--hub-url URL] call <tool> [json]
  clawctl-operator [--hub-url URL] tools
  clawctl-operator version

Identity is the Tailscale node this process runs on. Hub WhoIs the TCP source.
This command sends no Authorization header and does not open the Hub database.
CLAWCTL_HUB_URL is the same origin when --hub-url is omitted.
Write tools need preview_digest from a previous preview call.
`
