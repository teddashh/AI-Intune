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

	"github.com/teddashh/AI-Intune/internal/operator"
)

// cmdReportProfile 印出「我發佈了哪幾版 profile、誰身上是它、它點名的版本我指派過
// 沒有與看到過沒有」。
//
// ⚠⚠ 每機安裝狀態的分母是機器，所以一份一台都沒指派的 profile 在那一份報告上完全
// 不存在。這一份的分母是已發佈的 revision，它存在的唯一理由就是把那幾列講出來。
func cmdReportProfile(argv []string) {
	if err := runProfileReportCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runProfileReportCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runProfileReportCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runProfileReportCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("report profile", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub report profile [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "  List which machines wear each published profile revision, which published revisions have zero assignments,")
		fmt.Fprintln(errOut, "  and whether each referenced package version was assigned or seen by this Hub.")
		fmt.Fprintln(errOut, "  discovery: --hub-url, CLAWCTL_HUB_URL, operator.json.")
		fs.PrintDefaults()
	}
	var hubURL auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (discovered automatically when omitted)")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "output safe UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("report profile: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("report profile: --json and --csv cannot be used together")
	}
	if hubURL.set {
		if err := validateReportChangeCLIText("hub-url", hubURL.value, 2048); err != nil {
			return errors.New(strings.NewReplacer("report changes:", "report profile:").Replace(err.Error()))
		}
	}
	client, err := reportHTTPClient("report profile", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	report, err := client.ProfileReport(ctx)
	if err != nil {
		return fmt.Errorf("failed to read profile report (HTTP operator API): %w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, report)
	}
	if csvOutput.value {
		body, err := operator.ReportCSV(operator.ProfileReportCSV(report))
		if err != nil {
			return fmt.Errorf("failed to generate profile report CSV: %w", err)
		}
		_, err = io.WriteString(out, body)
		return err
	}
	return writeProfileReport(out, report)
}

func writeProfileReport(out io.Writer, report operator.ProfileReport) error {
	if _, err := fmt.Fprintln(out, report.Headline); err != nil {
		return err
	}
	// ⚠ 這一句跟數字一起印，不是印在最後面。印在最後的限制，捲過去就沒有了。
	if _, err := fmt.Fprintln(out, report.Caveat); err != nil {
		return err
	}
	if err := writeProfileMisattributed(out, report); err != nil {
		return err
	}
	if err := writeProfileRevisions(out, report); err != nil {
		return err
	}
	if err := writeProfileStates(out, report); err != nil {
		return err
	}
	if err := writeProfilePackageStates(out, report); err != nil {
		return err
	}
	if err := writeProfileDetail(out, report); err != nil {
		return err
	}
	if report.NextStep == "" {
		return nil
	}
	_, err := fmt.Fprintf(out, "\n%s\n", report.NextStep)
	return err
}

// profileMisattributedCell 是一格「看得到」，而看到的那個版號量在一份沒有人在跑的
// 安裝上，連同它是哪一份 profile 點名的。
type profileMisattributedCell struct {
	profileID string
	revision  int64
	pkg       operator.ProfilePackage
}

// writeProfileMisattributed 排在已發佈那張表前面。
//
// ⚠⚠ 它講的是下面每一個「看到幾台」有多硬：那個數字是一個版號字串數出來的，而那個
// 字串可以是量在一份沒有人在跑的安裝上的。排在後面的話，一個只看前面兩張表的人會把
// 「指派過這一版，機隊上也看得到」讀成收工。
//
// ⚠ 這張表不印那一格自己的下一步。那一句講的是「指派過沒有／看到過沒有」，而
// 「指派過這一版，機隊上也看得到」那一格的下一句是空的——印成「—」會在這一節裡讀成
// 沒事要做，而這一節存在的理由剛好是它有事要做。
func writeProfileMisattributed(out io.Writer, report operator.ProfileReport) error {
	if report.SeenMisattributed == 0 {
		return nil
	}
	cells := make([]profileMisattributedCell, 0, report.SeenMisattributed)
	for _, row := range report.Profiles {
		for _, pkg := range row.Packages {
			if pkg.SeenMisattributedOn > 0 {
				cells = append(cells, profileMisattributedCell{
					profileID: row.ProfileID, revision: row.Revision, pkg: pkg,
				})
			}
		}
	}
	if _, err := fmt.Fprintf(out, "\n%d visible versions measure an installation that is not running\n",
		len(cells)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "profile\trevision\tpackage\tversion\tseen on\tmeasured wrong\twhat this version is on this Hub")
	for _, cell := range cells {
		fmt.Fprintf(table, "%s\t%d\t%s\t%s\t%d\t%d\t%s\n",
			cell.profileID, cell.revision, cell.pkg.PackageID, cell.pkg.Version,
			cell.pkg.SeenOn, cell.pkg.SeenMisattributedOn, cell.pkg.Title)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out,
		"To see which file those machines measured, inspect the per-machine install report.")
	return err
}

// writeProfileRevisions 一版一列，而且那兩個台數要分開。
//
// ⚠ 「機隊上穿著它的台數」與「已退役卻還穿著它的台數」併成一個數字的話，一份只有
// 退役機器還穿著它的 profile 在 terminal 上看起來還在用。
func writeProfileRevisions(out io.Writer, report operator.ProfileReport) error {
	if len(report.Profiles) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d published profile revisions\n", len(report.Profiles)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "profile\trevision\tstate\tin fleet\tretired\tpublished at\tnext step")
	for _, row := range report.Profiles {
		fmt.Fprintf(table, "%s\t%d\t%s\t%d\t%d\t%s\t%s\n",
			row.ProfileID, row.Revision, row.Title, row.AssignedOn, row.RetiredOn,
			row.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"), profileCellOrDash(row.NextStep))
	}
	return table.Flush()
}

// writeProfileStates / writeProfilePackageStates 是「現在卡在哪幾種狀態」那兩段。
//
// ⚠ 只印真的有列的那幾種。一份四列、其中三列是 0 的表，會把真正有列的那一列埋掉；
// 每一種狀態各自是什麼意思，--json 帶得走完整一份。
func writeProfileStates(out io.Writer, report operator.ProfileReport) error {
	rows := make([]operator.ProfileStateCount, 0, len(report.States))
	for _, state := range report.States {
		if state.Count > 0 {
			rows = append(rows, state)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d states\n", len(rows)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "state\trevisions\tmeaning\tnext step")
	for _, state := range rows {
		fmt.Fprintf(table, "%s\t%d\t%s\t%s\n",
			state.Title, state.Count, state.Meaning, profileCellOrDash(state.NextStep))
	}
	return table.Flush()
}

func writeProfilePackageStates(out io.Writer, report operator.ProfileReport) error {
	rows := make([]operator.ProfilePackageStateCount, 0, len(report.PackageStates))
	for _, state := range report.PackageStates {
		if state.Count > 0 {
			rows = append(rows, state)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d package states\n", len(rows)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "what this version is on this Hub\tcells\tmeaning\tnext step")
	for _, state := range rows {
		fmt.Fprintf(table, "%s\t%d\t%s\t%s\n",
			state.Title, state.Count, state.Meaning, profileCellOrDash(state.NextStep))
	}
	return table.Flush()
}

// writeProfileDetail 一版一段：它點名了什麼，以及誰身上是它。
//
// ⚠ 每一版都印，包含「已經發佈到更新的版本」那幾版。只印要人動手的那幾版，等於讓
// 這份報告變成一張待辦清單——而它回答的是「發佈了，然後呢」，那句話對每一版都有答案。
//
// ⚠⚠ 「指派過幾次」與「機隊上看到幾台」兩欄都印。兩個軸合成一個「有沒有」會把兩個
// 相反的發現寫進同一格：指派過卻沒有一台回報它，是工作單那一側的事；沒有指派過卻
// 看得到，是有人從指派以外的路徑裝上去的。
//
// ⚠ 「其中量錯」緊跟在「看到幾台」後面。直接捲到某一份 profile 的人只看這一張表，而
// 那個台數少了這一欄就會被當成硬證據。它是一個整數，所以印在這張對齊過的表裡不會像
// 檔案路徑那樣把每一列撐開。
func writeProfileDetail(out io.Writer, report operator.ProfileReport) error {
	for _, row := range report.Profiles {
		if _, err := fmt.Fprintf(out, "\n%s\n", row.Headline); err != nil {
			return err
		}
		if err := writeProfilePackages(out, row); err != nil {
			return err
		}
		if err := writeProfileWearers(out, row); err != nil {
			return err
		}
	}
	return nil
}

func writeProfilePackages(out io.Writer, row operator.ProfileRow) error {
	if len(row.Packages) == 0 {
		return nil
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "  package\tversion\twhat this version is on this Hub\tassignments\tlast assigned\tseen on\tmeasured wrong\tnext step")
	for _, pkg := range row.Packages {
		fmt.Fprintf(table, "  %s\t%s\t%s\t%d\t%s\t%d\t%d\t%s\n",
			pkg.PackageID, pkg.Version, pkg.Title, pkg.Intents,
			softwareMoment(pkg.LastAssignedAt), pkg.SeenOn, pkg.SeenMisattributedOn,
			profileCellOrDash(pkg.NextStep))
	}
	return table.Flush()
}

// writeProfileWearers 印出穿著這一版的機器，退役的那幾台標出來。
//
// ⚠ 少了那個標記，一份「只有已退役的機器身上還是這一版」的 profile 在 terminal 上
// 看起來還在用。
func writeProfileWearers(out io.Writer, row operator.ProfileRow) error {
	if len(row.Machines) == 0 {
		return nil
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "  machine\tin fleet\tassigned revision\tassigned at\tassigned by")
	for _, machine := range row.Machines {
		fmt.Fprintf(table, "  %s\t%s\t%d\t%s\t%s\n",
			machine.DisplayName, profileFleetCell(machine.Retired), machine.AssignmentRevision,
			machine.AssignedAt.UTC().Format("2006-01-02T15:04:05Z"), machine.AssignedBy)
	}
	return table.Flush()
}

func profileFleetCell(retired bool) string {
	if retired {
		return "retired"
	}
	return "in fleet"
}

func profileCellOrDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
