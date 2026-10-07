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
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func cmdReport(argv []string) {
	if err := runDailyReportCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runDailyReportCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runDailyReportCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runDailyReportCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub report [--since DURATION] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  Show the daily morning report that would be generated now; does not send or record notifications.")
		fmt.Fprintln(errOut, "  Standard mode uses the HTTP operator API; --db is only for fenced break-glass when the Hub is fully stopped.")
		fs.PrintDefaults()
	}
	var hubURL, dbPath auditStringFlag
	since := auditStringFlag{value: operator.DefaultDailyReportWindow.String()}
	listen := auditStringFlag{value: os.Getenv("CLAWCTL_LISTEN")}
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (auto-discovered when omitted)")
	fs.Var(&dbPath, "db", "path to existing SQLite file for stopped-service direct DB break-glass")
	fs.Var(&since, "since", "compare against duration ago (whole seconds, 1s..720h)")
	fs.Var(&listen, "listen", "Hub listen address used for daily report links in direct mode (defaults to $CLAWCTL_LISTEN)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("report: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set && dbPath.set {
		return errors.New("report: --hub-url (HTTP mode) and --db (direct mode) cannot be specified together")
	}
	if listen.set && !dbPath.set {
		return errors.New("report: --listen is only valid in stopped-service direct mode with --db specified")
	}
	for _, field := range []struct {
		name  string
		value auditStringFlag
		max   int
	}{
		{"hub-url", hubURL, 2048}, {"db", dbPath, 4096}, {"since", since, 64}, {"listen", listen, 2048},
	} {
		if field.value.set || field.name == "since" {
			if !validOperatorChangeQueryText(field.value.value, field.max) {
				return fmt.Errorf("report: --%s cannot be empty, contain leading or trailing whitespace, or control characters", field.name)
			}
		}
	}
	window, err := time.ParseDuration(since.value)
	if err != nil || window%time.Second != 0 || window < time.Second || window > operator.MaxDailyReportWindow {
		return fmt.Errorf("report: --since must be a whole-second duration from 1s to %s", operator.MaxDailyReportWindow)
	}
	if dbPath.set {
		return runDailyReportDirect(ctx, dbPath.value, listen.value, window, out, errOut, deps)
	}
	client, err := reportHTTPClient("report", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	result, err := client.DailyReport(ctx, window)
	if err != nil {
		return fmt.Errorf("failed to read daily report (HTTP operator API): %w", err)
	}
	_, err = io.WriteString(out, result.Body)
	return err
}

func runDailyReportDirect(ctx context.Context, dbPath, listen string, window time.Duration,
	out, errOut io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "report", dbPath, deps, func(st *store.Store) error {
		loadExpectations(st)
		if _, err := st.CurrentWorkloadPolicyToken("operator-daily-report-read-policy-proof"); err != nil {
			return fmt.Errorf("report: shell CLAWCTL_EXPECTATIONS does not match workload policy last published by Hub; direct DB decision rejected: %w", err)
		}
		h := &hub{store: st, drillStamp: drillStampPath(dbPath)}
		var why string
		if h.publicURL, why = publicBase(listen); why != "" {
			fmt.Fprintf(errOut, "(daily report will not include links: %s)\n", why)
		}
		result, err := h.dailyReportAt(time.Now().UTC().Truncate(time.Second), window)
		if err != nil {
			return fmt.Errorf("failed to generate daily report (direct DB): %w", err)
		}
		_, err = io.WriteString(out, result.Body)
		return err
	})
}
