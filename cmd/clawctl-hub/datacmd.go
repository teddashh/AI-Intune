package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// cmdDataDisclosure 印出這個 Hub 對每一台機器留了什麼。
//
// 保留期那一頁講得出「observed_state 留 30 天」，但講不出 observed_state 裡面是
// 什麼、那些字是誰寫的。這一份講得出來。
func cmdDataDisclosure(argv []string) {
	if err := runDataDisclosureCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runDataDisclosureCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runDataDisclosureCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runDataDisclosureCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("data", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub data [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  lists each category of data this Hub retains for each machine: who produced it, how long it is kept, and what remains after retirement")
		fmt.Fprintln(errOut, "  discovery: --hub-url, CLAWCTL_HUB_URL, operator.json")
		fs.PrintDefaults()
	}
	var hubURL auditStringFlag
	var jsonOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (auto-discovered if omitted)")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("data: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set {
		if err := validateReportChangeCLIText("hub-url", hubURL.value, 2048); err != nil {
			return errors.New(strings.NewReplacer("report changes:", "data:").Replace(err.Error()))
		}
	}
	client, err := reportHTTPClient("data", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	disclosure, err := client.DataDisclosure(ctx)
	if err != nil {
		return fmt.Errorf("failed to read data disclosure (HTTP operator API): %w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, disclosure)
	}
	return writeDataDisclosure(out, disclosure)
}

func writeDataDisclosure(out io.Writer, disclosure operator.DataDisclosure) error {
	if _, err := fmt.Fprintf(out,
		"this Hub retains %d data categories across %d tables for each machine; %d categories are pruned by retention\n",
		len(disclosure.Categories), disclosure.Tables, disclosure.Timed); err != nil {
		return err
	}
	// 「留的是什麼」沒有長度上限，所以它跟其餘的交代一起放在不對齊的最後一格。
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "CATEGORY\tRETENTION\tWHERE TO SEE IT\t"); err != nil {
		return err
	}
	for _, category := range disclosure.Categories {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t\n",
			category.Title, category.RetentionSentence, category.Path); err != nil {
			return err
		}
		for _, line := range []struct{ label, value string }{
			{"source", category.SourceSentence},
			{"what is held", category.Holds},
			{"free text", category.FreeTextSentence},
			{"after retirement", category.RetirementSentence},
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

// runMachineDataSubcommand 印出 Hub 現在替一台機器留著多少列。
func runMachineDataSubcommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("machine data", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine data --machine <id> [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "  lists row counts, oldest and newest timestamps, and current prune cutoffs for this machine across categories in the Hub")
		fmt.Fprintln(errOut, "  discovery: --hub-url, CLAWCTL_HUB_URL, operator.json")
		fs.PrintDefaults()
	}
	var machine, hubURL auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&machine, "machine", "machine_id")
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (auto-discovered if omitted)")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "output safe UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine data: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(machine.value) == "" {
		return errors.New("machine data: --machine is required")
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("machine data: cannot use both --json and --csv")
	}
	for _, field := range []struct {
		name  string
		value auditStringFlag
		max   int
	}{{"machine", machine, 256}, {"hub-url", hubURL, 2048}} {
		if field.value.set {
			if err := validateReportChangeCLIText(field.name, field.value.value, field.max); err != nil {
				return errors.New(strings.NewReplacer(
					"report changes:", "machine data:").Replace(err.Error()))
			}
		}
	}
	client, err := reportHTTPClient("machine data", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	result, err := client.MachineData(ctx, machine.value)
	if err != nil {
		return fmt.Errorf("failed to read machine data disclosure (HTTP operator API): %w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, result)
	}
	if csvOutput.value {
		body, err := operator.ReportCSV(operator.MachineDataCSV(result))
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, body)
		return err
	}
	return writeMachineData(out, result)
}

func writeMachineData(out io.Writer, result operator.MachineDataResult) error {
	lifecycle := "still checking in, will continue to increase"
	if result.Retired {
		lifecycle = "retired, no new rows will be added"
	}
	if _, err := fmt.Fprintf(out, "%s (%s) has %d total rows in the Hub; %s\n",
		result.DisplayName, result.MachineID, result.Rows, lifecycle); err != nil {
		return err
	}
	if result.Undated > 0 {
		if _, err := fmt.Fprintf(out,
			"%d rows have no Hub received timestamp; they are retained but cannot be ordered into oldest / newest\n",
			result.Undated); err != nil {
			return err
		}
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "CATEGORY\tROWS\tOLDEST (UTC)\tNEWEST (UTC)\tRETENTION\t"); err != nil {
		return err
	}
	for _, measured := range result.Categories {
		if _, err := fmt.Fprintf(table, "%s\t%d\t%s\t%s\t%s\t\n",
			measured.Category.Title, measured.Rows,
			machineDataMoment(measured.Oldest), machineDataMoment(measured.Newest),
			measured.Category.RetentionSentence); err != nil {
			return err
		}
		if measured.CutoffAt != nil {
			if _, err := fmt.Fprintf(table, "\t\t\t\t\tcurrently pruning rows received before %s\n",
				operator.ReportCSVOptionalTime(measured.CutoffAt)); err != nil {
				return err
			}
		}
		if measured.Undated > 0 {
			if _, err := fmt.Fprintf(table, "\t\t\t\t\t%d rows have no Hub received timestamp\n",
				measured.Undated); err != nil {
				return err
			}
		}
	}
	return table.Flush()
}

func machineDataMoment(value *time.Time) string {
	if value == nil {
		return "—"
	}
	return value.Format(time.RFC3339)
}
