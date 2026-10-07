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

// runMachineTimelineSubcommand 印出一台機器的事件時間軸。
//
// 每一列都來自 Hub 自己寫下的紀錄：名冊、健康判定、工作單與操作員動作。每一個
// 來源都各自交代它在這段期間讀到幾列、有沒有讀完，所以一個總數不會被當成
// 「這段期間就只發生了這些」。
func runMachineTimelineSubcommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("machine timeline", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintf(errOut, "Usage: clawctl-hub machine timeline --machine <id> [--days 1..%d] [--json | --csv] [--hub-url URL]\n",
			operator.MaxMachineTimelineDays)
		fmt.Fprintln(errOut, "  List roster, health verdicts, tickets, and operator actions from newest to oldest, reporting how much each source read.")
		fmt.Fprintln(errOut, "  discovery: --hub-url, CLAWCTL_HUB_URL, operator.json.")
		fs.PrintDefaults()
	}
	days := ticketDaysFlag{}
	var machine, hubURL auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&machine, "machine", "machine_id")
	fs.Var(&days, "days", fmt.Sprintf("last N days (1..%d; default %d)",
		operator.MaxMachineTimelineDays, operator.DefaultMachineTimelineDays))
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (auto-discovered if omitted)")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "output safe UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine timeline: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(machine.value) == "" {
		return errors.New("machine timeline: --machine is required")
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("machine timeline: --json and --csv cannot be used together")
	}
	for _, field := range []struct {
		name  string
		value auditStringFlag
		max   int
	}{{"machine", machine, 256}, {"hub-url", hubURL, 2048}} {
		if field.value.set {
			if err := validateReportChangeCLIText(field.name, field.value.value, field.max); err != nil {
				return errors.New(strings.NewReplacer(
					"report changes:", "machine timeline:").Replace(err.Error()))
			}
		}
	}
	if days.set && (days.value < 1 || days.value > operator.MaxMachineTimelineDays) {
		return fmt.Errorf("machine timeline: --days must be between 1 and %d", operator.MaxMachineTimelineDays)
	}
	client, err := reportHTTPClient("machine timeline", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	result, err := client.MachineTimeline(ctx, machine.value, days.value)
	if err != nil {
		return fmt.Errorf("failed to read machine event timeline (HTTP operator API): %w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, result)
	}
	if csvOutput.value {
		body, err := operator.ReportCSV(operator.MachineTimelineCSV(result))
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, body)
		return err
	}
	return writeMachineTimeline(out, result)
}

func writeMachineTimeline(out io.Writer, result operator.MachineTimelineResult) error {
	if _, err := fmt.Fprintf(out, "%s (%s) last %d days, %d rows total; %s → %s (UTC).\n",
		result.DisplayName, result.MachineID, result.Window.Days, result.Total,
		result.Window.From.Format(time.RFC3339), result.Window.To.Format(time.RFC3339)); err != nil {
		return err
	}
	sources := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(sources, "SOURCE\tROWS\tEVIDENCE READ"); err != nil {
		return err
	}
	for _, read := range result.Sources {
		if _, err := fmt.Fprintf(sources, "%s\t%d\t%s\n",
			read.Label, read.Count, read.Evidence); err != nil {
			return err
		}
	}
	if err := sources.Flush(); err != nil {
		return err
	}
	for _, read := range result.Sources {
		if read.Complete {
			continue
		}
		if _, err := fmt.Fprintf(out, "%s did not finish reading this period. Next step: %s\n",
			read.Label, read.NextStep); err != nil {
			return err
		}
	}
	if len(result.Entries) == 0 {
		_, err := fmt.Fprintln(out, "Hub recorded no events for this machine during this period.")
		return err
	}
	// 說明與證據放在不對齊的最後一格。狀態判定的理由沒有長度上限，把它放進一個
	// 對齊的欄位，一列長理由就會把整張表撐到讀不出先後順序——而先後順序正是這一頁
	// 存在的理由。
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "TIME (UTC)\tSOURCE\tWHAT HAPPENED\t"); err != nil {
		return err
	}
	for _, entry := range result.Entries {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t\n",
			entry.At.Format(time.RFC3339), operator.MachineTimelineSourceLabel(entry.Source),
			entry.Summary); err != nil {
			return err
		}
		for _, line := range []struct{ label, value string }{
			{"Detail", entry.Detail}, {"Evidence", entry.Href},
		} {
			if line.value == "" {
				continue
			}
			if _, err := fmt.Fprintf(table, "\t\t\t%s: %s\n", line.label, line.value); err != nil {
				return err
			}
		}
	}
	return table.Flush()
}
