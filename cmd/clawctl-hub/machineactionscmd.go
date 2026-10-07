package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// runMachineActionsSubcommand 印出這台機器現在能做什麼。
//
// 清單只列這個操作員的 capability 涵蓋得到的動作，跟 Web 與 JSON 完全一樣：Hub
// 那邊已經濾過了，CLI 不再自己加一層「你沒權限」的說明。
func runMachineActionsSubcommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("machine actions", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine actions --machine <id> [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  List actions currently available for this machine, their consequences, blocking reasons, and next steps.")
		fmt.Fprintln(errOut, "  discovery: --hub-url, CLAWCTL_HUB_URL, operator.json.")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "machine_id")
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine actions: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*machine) == "" {
		return errors.New("machine actions: --machine is required")
	}
	explicit := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "hub-url" {
			explicit = true
		}
	})
	client, err := machinesHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return fmt.Errorf("machine actions: %w", err)
	}
	catalogue, err := client.MachineActions(ctx, *machine)
	if err != nil {
		return fmt.Errorf("failed to read device action catalogue (HTTP operator API): %w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, catalogue)
	}
	return writeMachineActions(out, catalogue)
}

func writeMachineActions(out io.Writer, catalogue operator.MachineActionCatalogue) error {
	fmt.Fprintf(out, "%s (%s) %s; evaluated at %s (state changes over time)\n",
		catalogue.DisplayName, catalogue.MachineID,
		machineActionsStateLabel(catalogue.State),
		catalogue.EvaluatedAt.Local().Format(time.RFC3339))
	if len(catalogue.Actions) == 0 {
		fmt.Fprintln(out, "This credential does not cover any device actions.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tSTATE\tEFFECT")
	for _, action := range catalogue.Actions {
		state := "available"
		if !action.Available {
			state = "blocked"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", action.Label, state, action.Effect)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, action := range catalogue.Actions {
		if action.Available {
			continue
		}
		fmt.Fprintf(out, "%s: %s", action.Label, action.Situation)
		if action.NextStep != "" {
			fmt.Fprintf(out, "  Next step: %s", action.NextStep)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "%d available, %d blocked.\n", catalogue.Available, catalogue.Blocked)
	return nil
}

func machineActionsStateLabel(state operator.MachineLifecycleState) string {
	if state == operator.MachineLifecycleStateRetired {
		return "retired"
	}
	return "active"
}
