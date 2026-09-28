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
		fmt.Fprintln(errOut, "用法：clawctl-hub machine actions --machine <id> [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  列出這台機器現在可以做的動作、各自的後果，以及被擋住的原因與下一步。")
		fmt.Fprintln(errOut, "  discovery：--hub-url、CLAWCTL_HUB_URL、operator.json。")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "machine_id")
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine actions: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*machine) == "" {
		return errors.New("machine actions: --machine 必填")
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
		return fmt.Errorf("讀取裝置動作目錄失敗（HTTP operator API）：%w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, catalogue)
	}
	return writeMachineActions(out, catalogue)
}

func writeMachineActions(out io.Writer, catalogue operator.MachineActionCatalogue) error {
	fmt.Fprintf(out, "%s（%s）%s；判定時間 %s（狀態會隨時間改變）\n",
		catalogue.DisplayName, catalogue.MachineID,
		machineActionsStateLabel(catalogue.State),
		catalogue.EvaluatedAt.Local().Format(time.RFC3339))
	if len(catalogue.Actions) == 0 {
		fmt.Fprintln(out, "這把憑證沒有涵蓋任何裝置動作。")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tSTATE\tEFFECT")
	for _, action := range catalogue.Actions {
		state := "可用"
		if !action.Available {
			state = "被擋"
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
		fmt.Fprintf(out, "%s：%s", action.Label, action.Situation)
		if action.NextStep != "" {
			fmt.Fprintf(out, "　下一步：%s", action.NextStep)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "現在可用 %d 項，被擋 %d 項。\n", catalogue.Available, catalogue.Blocked)
	return nil
}

func machineActionsStateLabel(state operator.MachineLifecycleState) string {
	if state == operator.MachineLifecycleStateRetired {
		return "已退役"
	}
	return "服役中"
}
