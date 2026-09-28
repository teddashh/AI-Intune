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

// cmdReportInstall 印出「我叫哪幾台裝什麼、我在它們上面看到什麼、哪幾格對不起來」。
//
// ⚠⚠ 它比的是兩件 Hub 自己知道的事：最後一筆指派，跟最新一筆觀測。它講不出「這台
// 裝失敗了」——機器上的東西可以從指派以外的路徑裝上去，而這個 Hub 看不到那條路。
// 那句限制 terminal 上也要講：一列「指派的比看到的舊」少了它，會被讀成有人亂動這台。
func cmdReportInstall(argv []string) {
	if err := runInstallReportCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runInstallReportCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runInstallReportCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runInstallReportCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("report install", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub report install [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "  列出每一台最後被指派裝什麼、Hub 在它上面看到什麼、哪幾台對不起來、哪幾台沒被指派過。")
		fmt.Fprintln(errOut, "  discovery：--hub-url、CLAWCTL_HUB_URL、operator.json。")
		fs.PrintDefaults()
	}
	var hubURL auditStringFlag
	var jsonOutput, csvOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&jsonOutput, "json", "輸出 stable operator JSON DTO")
	fs.Var(&csvOutput, "csv", "輸出安全的 UTF-8 CSV")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("report install: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("report install: --json 與 --csv 不可同時使用")
	}
	if hubURL.set {
		if err := validateReportChangeCLIText("hub-url", hubURL.value, 2048); err != nil {
			return errors.New(strings.NewReplacer("report changes:", "report install:").Replace(err.Error()))
		}
	}
	client, err := reportHTTPClient("report install", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	report, err := client.InstallReport(ctx)
	if err != nil {
		return fmt.Errorf("讀取每機安裝狀態失敗（HTTP operator API）：%w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, report)
	}
	if csvOutput.value {
		body, err := operator.ReportCSV(operator.InstallReportCSV(report))
		if err != nil {
			return fmt.Errorf("產生每機安裝狀態 CSV 失敗：%w", err)
		}
		_, err = io.WriteString(out, body)
		return err
	}
	return writeInstallReport(out, report)
}

func writeInstallReport(out io.Writer, report operator.InstallReport) error {
	if _, err := fmt.Fprintln(out, report.Headline); err != nil {
		return err
	}
	// ⚠ 這一句跟數字一起印，不是印在最後面。印在最後的限制，捲過去就沒有了。
	if _, err := fmt.Fprintln(out, report.Caveat); err != nil {
		return err
	}
	// ⚠⚠ 這一段印在資源那張表前面。那張表上「不一樣」那一欄是有人會直接照著動手的
	// 數字，而它比的可能是一個沒有人在跑的檔案——先講在比哪一個檔案，再講誰對不起來。
	if err := writeInstallMisattributed(out, report); err != nil {
		return err
	}
	if err := writeInstallResources(out, report); err != nil {
		return err
	}
	if err := writeInstallStates(out, report); err != nil {
		return err
	}
	if err := writeInstallRuntimes(out, report); err != nil {
		return err
	}
	if err := writeInstallMatrix(out, report); err != nil {
		return err
	}
	if report.NextStep == "" {
		return nil
	}
	_, err := fmt.Fprintf(out, "\n%s\n", report.NextStep)
	return err
}

// writeInstallMisattributed 是「看到的那個版號，量的不是正在跑的那一份」那幾格。
//
// ⚠ 那兩個路徑印在這裡，不印在下面那張對齊的全表上。一個路徑沒有長度上限，塞進那張
// 表會把每一列都撐開，然後整張表要橫向捲——而那張表的價值全在於一眼看完。
func writeInstallMisattributed(out io.Writer, report operator.InstallReport) error {
	if report.Misattributed == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d 格看到的版號量的不是正在跑的那一份\n",
		report.Misattributed); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "資源\t機器\t指派的\t看到的\t這一格是什麼\t量版號的那個檔案\t正在跑的那個檔案\t下一步")
	for _, resource := range report.Resources {
		for _, row := range resource.Rows {
			if row.Runtime == nil || !operator.ToolRuntimeMisattributed(row.Runtime.State) {
				continue
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				resource.Name, row.DisplayName, installCellOrDash(row.Assigned),
				installCellOrDash(row.Observed), row.Runtime.Title,
				installCellOrDash(row.Runtime.MeasuredFile),
				installCellOrDash(row.Runtime.RunningFile), row.Runtime.NextStep)
		}
	}
	return table.Flush()
}

// writeInstallResources 一個資源一列，而且那幾個數字要當場加得起來：
// 被指派過＝一樣＋不一樣＋對不起來，被指派過＋沒被指派過＝分母。
//
// ⚠ 少了「對不起來」那一欄，被指派過就會比一樣加不一樣多出幾台，而讀的人只會以為
// 是自己算錯。那幾台是真的存在的——指派沒講版號、裝了但問不到版號、回報說它上面
// 沒有、回報裡根本沒有這個東西、從來沒回報過。
func writeInstallResources(out io.Writer, report operator.InstallReport) error {
	if len(report.Resources) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d 個資源\n", len(report.Resources)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "資源\t被指派過\t一樣\t不一樣\t對不起來\t版號不是跑的那一份\t沒被指派過\t下一步")
	for _, resource := range report.Resources {
		unknown := resource.AssignedOn - resource.MatchingOn - resource.DifferingOn
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			resource.Name, resource.AssignedOn, resource.MatchingOn, resource.DifferingOn,
			unknown, resource.MisattributedOn, resource.UnassignedOn, resource.NextStep)
	}
	return table.Flush()
}

// writeInstallStates 是「這個機隊現在卡在哪幾種狀態」那一段。
//
// ⚠ 只印真的有格子的那幾種。一份十列、其中八列是 0 的表，會把真正有格子的那兩列
// 埋掉；每一種狀態各自是什麼意思，--json 帶得走完整一份。
func writeInstallStates(out io.Writer, report operator.InstallReport) error {
	rows := make([]operator.InstallStateCount, 0, len(report.States))
	for _, state := range report.States {
		if state.Count > 0 {
			rows = append(rows, state)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d 種狀態\n", len(rows)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "這一格是什麼\t幾格\t意思\t下一步")
	for _, state := range rows {
		fmt.Fprintf(table, "%s\t%d\t%s\t%s\n", state.Title, state.Count, state.Meaning, state.NextStep)
	}
	return table.Flush()
}

// writeInstallRuntimes 是「看到的那個版號講的是哪一份」那六種各有幾格。
//
// ⚠ 跟狀態那一段一樣，只印真的有格子的那幾種——六列裡四列是 0 的表會把有格子的那兩
// 列埋掉。哪幾種算「量錯了檔案」，最後那一句要講出來：六種都印成一列的話，看起來像
// 六件都要人動手。
func writeInstallRuntimes(out io.Writer, report operator.InstallReport) error {
	rows := make([]operator.ToolRuntimeCount, 0, len(report.RuntimeStates))
	for _, state := range report.RuntimeStates {
		if state.Count > 0 {
			rows = append(rows, state)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n看到的版號講的是哪一份：%d 種\n", len(rows)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "這一格是什麼\t幾格\t意思\t下一步")
	for _, state := range rows {
		fmt.Fprintf(table, "%s\t%d\t%s\t%s\n", state.Title, state.Count, state.Meaning, state.NextStep)
	}
	return table.Flush()
}

// writeInstallMatrix 是一個資源一段、一台機器一列的全表。
//
// ⚠ 每一台都印，包含「沒有被指派過」那幾台——它們在分母裡，而「這台從來沒有被指派
// 過這個東西」正是操作員最需要看到的一件事。只印被指派過的，等於讓指派過的變成分母。
//
// ⚠ 每一格自己的下一步也要印：「指派的比看到的舊」跟「指派的比看到的新」在畫面上只
// 差一個字，前者要人決定哪一邊才對，後者只是去看工作單走到哪裡。
func writeInstallMatrix(out io.Writer, report operator.InstallReport) error {
	for _, resource := range report.Resources {
		if _, err := fmt.Fprintf(out, "\n%s\n", resource.Headline); err != nil {
			return err
		}
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "機器\t這一格是什麼\t指派的\t看到的\t版號講的是哪一份\t指派來源\tHub 收到的時刻\t下一步")
		for _, row := range resource.Rows {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				row.DisplayName, row.Title, installCellOrDash(row.Assigned),
				installObservedCell(row), installRowRuntime(row),
				installCellOrDash(row.ScopeLabel),
				softwareMoment(row.ObservedAt), row.NextStep)
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// installObservedCell 把「這個版號是哪裡來的」講清楚。
//
// ⚠ 「它自己講的」跟「我從檔案裡讀的」是兩件事：程式壞掉的時候，檔案上那個版號會
// 跟真正跑起來的那一版不一樣，而這一列會拿它去跟指派的比。
func installObservedCell(row operator.InstallRow) string {
	if row.Observed == "" {
		return "—"
	}
	if row.FromDisk {
		return row.Observed + "（檔案上讀的）"
	}
	return row.Observed
}

// installRowRuntime 是那張對齊的表上「版號講的是哪一份」那一格。
//
// ⚠ 只印那一句標題，不印路徑。那一格留白的意思是「Hub 沒有在這台上量到這個東西」，
// 跟「沒有找到在跑它的 process」是兩件事——後者是一句有內容的話，會印出來。
func installRowRuntime(row operator.InstallRow) string {
	if row.Runtime == nil {
		return "—"
	}
	return row.Runtime.Title
}

func installCellOrDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
