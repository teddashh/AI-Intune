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
		fmt.Fprintln(errOut, "用法：clawctl-hub data [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  列出這個 Hub 對每一台機器留的每一類資料：是誰產生的、留多久、退役之後還剩什麼。")
		fmt.Fprintln(errOut, "  discovery：--hub-url、CLAWCTL_HUB_URL、operator.json。")
		fs.PrintDefaults()
	}
	var hubURL auditStringFlag
	var jsonOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&jsonOutput, "json", "輸出 stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("data: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
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
		return fmt.Errorf("讀取資料揭露面失敗（HTTP operator API）：%w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, disclosure)
	}
	return writeDataDisclosure(out, disclosure)
}

func writeDataDisclosure(out io.Writer, disclosure operator.DataDisclosure) error {
	if _, err := fmt.Fprintf(out,
		"這個 Hub 對每一台機器留 %d 類資料，涵蓋 %d 張表；其中 %d 類會被保留期清掉。\n",
		len(disclosure.Categories), disclosure.Tables, disclosure.Timed); err != nil {
		return err
	}
	// 「留的是什麼」沒有長度上限，所以它跟其餘的交代一起放在不對齊的最後一格。
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "類別\t留多久\t看得到它\t"); err != nil {
		return err
	}
	for _, category := range disclosure.Categories {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t\n",
			category.Title, category.RetentionSentence, category.Path); err != nil {
			return err
		}
		for _, line := range []struct{ label, value string }{
			{"來源", category.SourceSentence},
			{"留的是什麼", category.Holds},
			{"自由文字", category.FreeTextSentence},
			{"退役之後", category.RetirementSentence},
		} {
			if line.value == "" {
				continue
			}
			if _, err := fmt.Fprintf(table, "\t\t\t%s：%s\n", line.label, line.value); err != nil {
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
		fmt.Fprintln(errOut, "用法：clawctl-hub machine data --machine <id> [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "  逐類列出這台機器在 Hub 裡的列數、最舊與最新，以及現在的清除界線。")
		fmt.Fprintln(errOut, "  discovery：--hub-url、CLAWCTL_HUB_URL、operator.json。")
		fs.PrintDefaults()
	}
	var machine, hubURL auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&machine, "machine", "machine_id")
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&jsonOutput, "json", "輸出 stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "輸出安全的 UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine data: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(machine.value) == "" {
		return errors.New("machine data: --machine 必填")
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("machine data: --json 與 --csv 不可同時使用")
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
		return fmt.Errorf("讀取單機資料揭露面失敗（HTTP operator API）：%w", err)
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
	lifecycle := "還在報到，會繼續增加"
	if result.Retired {
		lifecycle = "已退役，不會再有新的一列"
	}
	if _, err := fmt.Fprintf(out, "%s（%s）在 Hub 裡共 %d 列；%s。\n",
		result.DisplayName, result.MachineID, result.Rows, lifecycle); err != nil {
		return err
	}
	if result.Undated > 0 {
		if _, err := fmt.Fprintf(out,
			"其中 %d 列沒有 Hub 收到的時刻，它們仍然留著，只是排不進最舊 / 最新。\n",
			result.Undated); err != nil {
			return err
		}
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "類別\t列數\t最舊（UTC）\t最新（UTC）\t留多久\t"); err != nil {
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
			if _, err := fmt.Fprintf(table, "\t\t\t\t\t現在會清掉 %s 以前收到的列\n",
				operator.ReportCSVOptionalTime(measured.CutoffAt)); err != nil {
				return err
			}
		}
		if measured.Undated > 0 {
			if _, err := fmt.Fprintf(table, "\t\t\t\t\t%d 列沒有 Hub 收到的時刻\n",
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
