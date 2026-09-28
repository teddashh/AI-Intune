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

// cmdReportSoftware 印出「機隊上裝了什麼、各是哪一版、哪幾台沒有」。
//
// ⚠⚠ 它講得出口的只有「這個機隊裡最新的是 X，這一台是 Y」，不是「這一台該升級
// 了」——後面那句話需要一個這個 Hub 沒有的事實。那句限制 terminal 上也要講，不是
// 只有網頁講：一張全綠的表沒有那句話會被讀成「都是最新的」。
func cmdReportSoftware(argv []string) {
	if err := runSoftwareReportCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runSoftwareReportCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runSoftwareReportCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runSoftwareReportCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("report software", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub report software [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "  列出每一個工具裝在哪幾台、各是哪一版、哪幾台沒有、哪幾台沒回報過。")
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
		return fmt.Errorf("report software: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if jsonOutput.value && csvOutput.value {
		return errors.New("report software: --json 與 --csv 不可同時使用")
	}
	if hubURL.set {
		if err := validateReportChangeCLIText("hub-url", hubURL.value, 2048); err != nil {
			return errors.New(strings.NewReplacer("report changes:", "report software:").Replace(err.Error()))
		}
	}
	client, err := reportHTTPClient("report software", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	report, err := client.SoftwareReport(ctx)
	if err != nil {
		return fmt.Errorf("讀取軟體清查失敗（HTTP operator API）：%w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, report)
	}
	if csvOutput.value {
		body, err := operator.ReportCSV(operator.SoftwareReportCSV(report))
		if err != nil {
			return fmt.Errorf("產生軟體清查 CSV 失敗：%w", err)
		}
		_, err = io.WriteString(out, body)
		return err
	}
	return writeSoftwareReport(out, report)
}

func writeSoftwareReport(out io.Writer, report operator.SoftwareReport) error {
	if _, err := fmt.Fprintln(out, report.Headline); err != nil {
		return err
	}
	// ⚠ 這一句跟數字一起印，不是印在最後面。印在最後的限制，捲過去就沒有了。
	if _, err := fmt.Fprintln(out, report.Caveat); err != nil {
		return err
	}
	if err := writeSoftwareMisattributed(out, report); err != nil {
		return err
	}
	if err := writeSoftwareTools(out, report); err != nil {
		return err
	}
	if err := writeSoftwareMatrix(out, report); err != nil {
		return err
	}
	if report.NextStep == "" {
		return nil
	}
	_, err := fmt.Fprintf(out, "\n%s\n", report.NextStep)
	return err
}

// writeSoftwareMisattributed 印那幾格「版號講的不是正在跑的那一份」。
//
// ⚠⚠ 它印在工具那張表前面。後面每一個「機隊裡最新是 X，這一台是 Y」都假設那個版號
// 講的是機隊上真的在跑的東西；這幾格的版號量的是沒在跑的那一份，所以要先看到它們，
// 再往下讀。
//
// ⚠ 兩個檔案路徑都要印。一句「正在跑的是另一個檔案」沒有講出是哪兩個檔案的時候，
// 讀的人沒有辦法知道該去收掉哪一份。
func writeSoftwareMisattributed(out io.Writer, report operator.SoftwareReport) error {
	if report.Misattributed == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d 格的版號講的不是正在跑的那一份\n",
		report.Misattributed); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "工具\t機器\t版號\t這一格是什麼\t量版號的那個檔案\t正在跑的那個檔案\t下一步")
	for _, tool := range report.Tools {
		for _, row := range tool.Rows {
			if row.Runtime == nil || !operator.ToolRuntimeMisattributed(row.Runtime.State) {
				continue
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				tool.Name, row.DisplayName, softwareVersionOrDash(row.Version),
				row.Runtime.Title, softwareVersionOrDash(row.Runtime.MeasuredFile),
				softwareVersionOrDash(row.Runtime.RunningFile), row.Runtime.NextStep)
		}
	}
	return table.Flush()
}

// writeSoftwareTools 一個工具一列：幾台上有、幾台沒有、幾台沒回報過、看到哪幾版。
//
// ⚠ 「下一步」沒有長度上限，所以它走 tabwriter 不對齊的最後一格。對齊欄位裡只放
// 長度有界的東西，否則一句長句子會把每一列都推寬——包含那些不必看的列。
func writeSoftwareTools(out io.Writer, report operator.SoftwareReport) error {
	if len(report.Tools) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "\n%d 個工具\n", len(report.Tools)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "工具\t有\t沒有\t沒回報過\t機隊裡最新\t版號\t下一步")
	for _, tool := range report.Tools {
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			tool.Name, tool.InstalledOn, tool.AbsentOn, tool.UnreportedOn,
			softwareVersionOrDash(tool.Newest), softwareVersionSpread(tool), tool.NextStep)
	}
	return table.Flush()
}

// writeSoftwareMatrix 是一個工具一段、一台機器一列的全表。
//
// ⚠ 每一台都印，包含每一格都是「沒回報過」的那一台——它在分母裡，而它正是最該被
// 看到的那一台。只印有回報的，等於讓「有回報的」變成分母。
//
// ⚠ 每一格自己的下一步也要印：一格「沒回報過」跟一格「這台上沒有」在畫面上只差
// 幾個字，而它們要人做的事完全不同——前者去看那台的 agent，後者去裝東西。
func writeSoftwareMatrix(out io.Writer, report operator.SoftwareReport) error {
	for _, tool := range report.Tools {
		if _, err := fmt.Fprintf(out, "\n%s\n", tool.Headline); err != nil {
			return err
		}
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "機器\t這一格是什麼\t版號\t版號講的是哪一份\tHub 收到的時刻\t下一步")
		for _, row := range tool.Rows {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
				row.DisplayName, row.Title, softwareRowVersion(row), softwareRowRuntime(row),
				softwareMoment(row.ObservedAt), row.NextStep)
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// softwareVersionSpread 把「看到哪幾版、各幾台」壓成一格。
func softwareVersionSpread(tool operator.SoftwareTool) string {
	if len(tool.Versions) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(tool.Versions))
	for _, version := range tool.Versions {
		parts = append(parts, fmt.Sprintf("%s×%d", version.Version, version.Machines))
	}
	return strings.Join(parts, " ")
}

// softwareRowVersion 把一格上「版號是哪裡來的」講清楚。
//
// ⚠ 「它自己講的」跟「我從檔案裡讀的」是兩件事：工具壞掉的時候，檔案上那個版號
// 會跟真正跑起來的版本不一樣。
func softwareRowVersion(row operator.SoftwareRow) string {
	if row.Version == "" {
		return "—"
	}
	value := row.Version
	if row.FromDisk {
		value += "（檔案上讀的）"
	}
	if row.Shadowed {
		value += "（另有一份）"
	}
	return value
}

// softwareRowRuntime 是這一格的第二軸，壓成一格。
//
// ⚠ 這裡只放那一句標題，兩個檔案路徑留給上面那張錯歸因的表。對齊欄位裡放一個沒有
// 長度上限的路徑，會把每一台的每一列都推寬——包含那些不必看的列。
//
// ⚠ 沒有這一軸的那幾格印破折號，不是「沒有找到在跑它的 process」：那台上根本沒有
// 這個工具，所以沒有版號，也就沒有「這個版號講的是哪一份」可問。
func softwareRowRuntime(row operator.SoftwareRow) string {
	if row.Runtime == nil {
		return "—"
	}
	return row.Runtime.Title
}

func softwareVersionOrDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func softwareMoment(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "—"
	}
	return value.UTC().Format("2006-01-02T15:04:05Z")
}
