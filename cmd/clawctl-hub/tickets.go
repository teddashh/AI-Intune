package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type ticketDaysFlag struct {
	value int
	set   bool
}

func (value *ticketDaysFlag) String() string { return strconv.Itoa(value.value) }
func (value *ticketDaysFlag) Set(raw string) error {
	if value.set {
		return errors.New("不可重複")
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(parsed) != raw {
		return errors.New("必須是 canonical 十進位整數")
	}
	value.value, value.set = parsed, true
	return nil
}

func cmdTickets(argv []string) {
	if err := runTicketsCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runTicketsCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runTicketsCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runTicketsCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("tickets", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub tickets [--days 1..30] [--provider-ref sha256:…] [--json | --csv] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fmt.Fprintln(errOut, "  固定視窗採 Hub received_at 的 [from,to]；驗證成功需搭配驗證證據。")
		fs.PrintDefaults()
	}
	days := ticketDaysFlag{value: operator.DefaultTicketReadDays}
	var hubURL, dbPath, providerRef auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&days, "days", "最近幾天（1..30；預設 7）")
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&dbPath, "db", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	fs.Var(&providerRef, "provider-ref", "精確比對 opaque provider_ref")
	fs.Var(&jsonOutput, "json", "輸出 stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "輸出安全的 UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("tickets: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set && dbPath.set {
		return errors.New("tickets: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("tickets: --json 與 --csv 不可同時使用")
	}
	for _, field := range []struct {
		name  string
		value auditStringFlag
		max   int
	}{
		{"hub-url", hubURL, 2048}, {"db", dbPath, 4096}, {"provider-ref", providerRef, 71},
	} {
		if field.value.set {
			if err := validateReportChangeCLIText(field.name, field.value.value, field.max); err != nil {
				return errors.New(strings.NewReplacer("report changes:", "tickets:").Replace(err.Error()))
			}
		}
	}
	request := operator.TicketReadRequest{Days: days.value, ProviderRef: providerRef.value}
	if request.Days < 1 {
		return errors.New("tickets: --days 必須介於 1 與 30")
	}
	if err := operator.ValidateTicketReadRequest(request); err != nil {
		return fmt.Errorf("tickets: %w", err)
	}

	if dbPath.set {
		return runTicketsDirect(ctx, request, dbPath.value, jsonOutput.value, csvOutput.value, out, deps)
	}
	client, err := ticketsHTTPClient(hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	result, err := client.Tickets(ctx, request)
	if err != nil {
		return fmt.Errorf("讀取 tickets 失敗（HTTP operator API）：%w", err)
	}
	return writeTickets(out, result, jsonOutput.value, csvOutput.value, "HTTP operator API")
}

func ticketsHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if deps.newOperatorClient == nil {
		return nil, errors.New("tickets: operator HTTP client 未初始化")
	}
	if explicit {
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("tickets: 建立 HTTP operator client 失敗：%w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("tickets: Hub discovery 未初始化")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("tickets: 無法發現 Hub：%w", err)
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("tickets: 建立 discovered HTTP operator client 失敗：%w", err)
	}
	return client, nil
}

func runTicketsDirect(ctx context.Context, request operator.TicketReadRequest, dbPath string,
	jsonOutput, csvOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "tickets", dbPath, deps, func(st *store.Store) error {
		result, err := operator.New(st).ListTicketsContext(ctx, request, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("讀取 tickets 失敗（direct DB operator service）：%w", err)
		}
		return writeTickets(out, result, jsonOutput, csvOutput, "direct DB operator service")
	})
}

func writeTickets(out io.Writer, result operator.TicketReadResult, jsonOutput, csvOutput bool, source string) error {
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	if csvOutput {
		body, err := operator.ReportCSV(operator.TicketCSV(result))
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, body)
		return err
	}
	if _, err := fmt.Fprintf(out,
		"%s；Hub 評估時間 %s；固定視窗 %s %s → %s（%d 天，%s）。\n"+
			"報到率 %d%%（預期 %d 台，已回報 %d 台）；調度分析資格=%t。provider=%d，跑完回合=%d，有錯誤的回合=%d。\n"+
			"資料界線：provider 是上游文字；錯誤保留原文；驗證成功需搭配驗證證據。\n",
		source, result.EvaluatedAt.Format(time.RFC3339), result.Window.Boundary,
		result.Window.From.Format(time.RFC3339), result.Window.To.Format(time.RFC3339), result.Window.Days,
		result.Window.TimeBasis, result.Roster.ReportingRatePercent, result.Roster.Expected,
		result.Roster.Reporting, result.Roster.SchedulingEligible, result.MatchedTotal,
		result.TotalRuns(), result.TotalErrorRuns()); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		_, err := fmt.Fprintln(out, "沒有符合篩選條件的票證使用量記錄。")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "PROVIDER\t跑完回合\t尖峰/小時\t上次跑完（UTC）\t機器\t錯誤回合"); err != nil {
		return err
	}
	for _, item := range result.Items {
		last := "—"
		if item.LastRunAt != nil {
			last = item.LastRunAt.UTC().Format(time.RFC3339Nano)
		}
		machines := make([]string, 0, len(item.Machines.Items))
		for _, machine := range item.Machines.Items {
			machines = append(machines, machine.Text)
		}
		if _, err := fmt.Fprintf(table, "%s\t%d\t%d\t%s\t%s\t%d\n",
			terminalSafe(item.Provider.Text), item.Runs, item.PeakPerHour, last,
			terminalSafe(strings.Join(machines, ",")), item.ErrorRuns); err != nil {
			return err
		}
		for _, evidence := range item.Errors {
			if _, err := fmt.Fprintf(table, "  ×%d %s\t\t\t\t\t\n", evidence.Count,
				terminalSafe(evidence.Text.Text)); err != nil {
				return err
			}
		}
	}
	return table.Flush()
}
