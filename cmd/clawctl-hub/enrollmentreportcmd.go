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

// cmdReportEnrollment 印出「說好要納管的機器，來了沒有」。
//
// 名冊上一台開了票、票過期了、機器從來沒出現的列，跟一台正常運作的列在清單上
// 只差一盞燈。這一份把那個差別講成一句話，外加下一步。
func cmdReportEnrollment(argv []string) {
	if err := runEnrollmentReportCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runEnrollmentReportCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runEnrollmentReportCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runEnrollmentReportCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("report enrollment", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub report enrollment [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "  List what stage each roster row is at: whether tickets were used, whether the machine checked in, and what to do next.")
		fmt.Fprintln(errOut, "  discovery: --hub-url, CLAWCTL_HUB_URL, operator.json.")
		fs.PrintDefaults()
	}
	var hubURL auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (auto-discovered when omitted)")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "output safe UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("report enrollment: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("report enrollment: --json and --csv cannot be used together")
	}
	if hubURL.set {
		if err := validateReportChangeCLIText("hub-url", hubURL.value, 2048); err != nil {
			return errors.New(strings.NewReplacer("report changes:", "report enrollment:").Replace(err.Error()))
		}
	}
	client, err := reportHTTPClient("report enrollment", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	report, err := client.EnrollmentReport(ctx)
	if err != nil {
		return fmt.Errorf("failed to read enrollment report (HTTP operator API): %w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, report)
	}
	if csvOutput.value {
		body, err := operator.ReportCSV(operator.EnrollmentReportCSV(report))
		if err != nil {
			return fmt.Errorf("failed to generate enrollment report CSV: %w", err)
		}
		_, err = io.WriteString(out, body)
		return err
	}
	return writeEnrollmentReport(out, report)
}

func writeEnrollmentReport(out io.Writer, report operator.EnrollmentReport) error {
	arrival := "some machines have never checked in"
	if report.Arrived == report.Denominator {
		arrival = "every machine in denominator has checked in"
	}
	if _, err := fmt.Fprintf(out,
		"%d on roster, denominator %d machines (%d retired excluded); %d arrived, %d owed -- %s\n",
		report.Registered, report.Denominator, report.Retired,
		report.Arrived, report.Owed, arrival); err != nil {
		return err
	}
	if err := writeEnrollmentOwed(out, report); err != nil {
		return err
	}
	if err := writeEnrollmentRegister(out, report); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out,
		"There is only one way to leave the denominator: retirement. Revoking tickets or letting them expire will not remove a roster entry.")
	return err
}

// writeEnrollmentOwed 先講還沒到的那幾台，因為那是這份報告唯一需要人動手的部分。
//
// ⚠ 「下一步」沒有長度上限，所以它走 tabwriter 不對齊的最後一格。對齊欄位裡只放
// 長度有界的東西，否則一句長句子會把每一列都推寬——包含那些不必看的列。
func writeEnrollmentOwed(out io.Writer, report operator.EnrollmentReport) error {
	if report.Owed == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d machines owed\n", report.Owed); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "machine\tstage\tenrolled\tticket expires\tnext step")
	for _, row := range report.Rows {
		if !row.InDenominator || operator.EnrollmentStageArrived(row.Stage) {
			continue
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n",
			row.DisplayName, row.StageTitle, enrollmentMoment(&row.DeclaredAt),
			enrollmentMoment(row.TicketExpiresAt), row.NextStep)
	}
	return table.Flush()
}

func writeEnrollmentRegister(out io.Writer, report operator.EnrollmentReport) error {
	if _, err := fmt.Fprintf(out, "\n%d roster entries\n", report.Registered); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "machine\tstage\tenrolled\tticket expires\tlast check-in\t")
	for _, row := range report.Rows {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t\n",
			row.DisplayName, row.StageTitle,
			enrollmentMoment(&row.DeclaredAt), enrollmentMoment(row.TicketExpiresAt),
			enrollmentCheckinMoment(row.LastCheckinReceivedAt))
	}
	return table.Flush()
}

func enrollmentMoment(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "—"
	}
	return value.UTC().Format("2006-01-02T15:04:05Z")
}

func enrollmentCheckinMoment(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "never checked in"
	}
	return value.UTC().Format("2006-01-02T15:04:05Z")
}
