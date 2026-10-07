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
		return errors.New("cannot be repeated")
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(parsed) != raw {
		return errors.New("must be a canonical decimal integer")
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
		fmt.Fprintln(errOut, "Usage: clawctl-hub tickets [--days 1..30] [--provider-ref sha256:...] [--json | --csv] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  Normal mode uses the HTTP operator API; --db is fenced break-glass only when Hub is fully stopped.")
		fmt.Fprintln(errOut, "  Fixed window uses Hub received_at [from,to]; verification success requires verification evidence.")
		fs.PrintDefaults()
	}
	days := ticketDaysFlag{value: operator.DefaultTicketReadDays}
	var hubURL, dbPath, providerRef auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&days, "days", "recent days (1..30; default 7)")
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (discovered automatically when omitted)")
	fs.Var(&dbPath, "db", "existing SQLite file path for stopped-service direct DB break-glass")
	fs.Var(&providerRef, "provider-ref", "exact match for opaque provider_ref")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "output safe UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("tickets: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set && dbPath.set {
		return errors.New("tickets: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified")
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("tickets: --json and --csv cannot be used together")
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
		return errors.New("tickets: --days must be between 1 and 30")
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
		return fmt.Errorf("failed to read tickets (HTTP operator API): %w", err)
	}
	return writeTickets(out, result, jsonOutput.value, csvOutput.value, "HTTP operator API")
}

func ticketsHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if deps.newOperatorClient == nil {
		return nil, errors.New("tickets: operator HTTP client is not initialized")
	}
	if explicit {
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("tickets: failed to create HTTP operator client: %w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("tickets: Hub discovery is not initialized")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("tickets: unable to discover Hub: %w", err)
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("tickets: failed to create discovered HTTP operator client: %w", err)
	}
	return client, nil
}

func runTicketsDirect(ctx context.Context, request operator.TicketReadRequest, dbPath string,
	jsonOutput, csvOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "tickets", dbPath, deps, func(st *store.Store) error {
		result, err := operator.New(st).ListTicketsContext(ctx, request, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("failed to read tickets (direct DB operator service): %w", err)
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
		"%s; Hub evaluation time %s; fixed window %s %s -> %s (%d days, %s)\n"+
			"Reporting rate %d%% (expected %d, reported %d); scheduling analysis eligible=%t. provider=%d, completed runs=%d, error runs=%d\n"+
			"Data boundaries: provider is upstream text; errors preserve original text; verification success requires verification evidence\n",
		source, result.EvaluatedAt.Format(time.RFC3339), result.Window.Boundary,
		result.Window.From.Format(time.RFC3339), result.Window.To.Format(time.RFC3339), result.Window.Days,
		result.Window.TimeBasis, result.Roster.ReportingRatePercent, result.Roster.Expected,
		result.Roster.Reporting, result.Roster.SchedulingEligible, result.MatchedTotal,
		result.TotalRuns(), result.TotalErrorRuns()); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		_, err := fmt.Fprintln(out, "No ticket occupancy records matched the filter")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "PROVIDER\tRUNS\tPEAK/HOUR\tLAST RUN (UTC)\tMACHINES\tERROR RUNS"); err != nil {
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
