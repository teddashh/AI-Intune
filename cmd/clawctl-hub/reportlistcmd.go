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
	"strings"
	"text/tabwriter"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

func cmdReportList(argv []string) {
	if err := runReportListCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runReportListCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runReportListCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

// runReportListCommandWithDeps 印出這個 Hub 做得出哪些報告。
//
// 「看得到多遠」讀的是 Hub 現行的保留期，所以它只有走 Hub 才問得到：一份印著預設
// 保留期的清單，會在有人把保留期調短之後繼續承諾它給不出來的資料。
func runReportListCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("report list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub report list [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  列出這個 Hub 做得出的報告：各自回答什麼、範圍多大、看得到多遠、能不能整份匯出。")
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
		return fmt.Errorf("report list: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set {
		if err := validateReportChangeCLIText("hub-url", hubURL.value, 2048); err != nil {
			return errors.New(strings.NewReplacer("report changes:", "report list:").Replace(err.Error()))
		}
	}
	client, err := reportHTTPClient("report list", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	index, err := client.Reports(ctx)
	if err != nil {
		return fmt.Errorf("讀取報告清單失敗（HTTP operator API）：%w", err)
	}
	if jsonOutput.value {
		return writeOperatorJSON(out, index)
	}
	return writeReportIndex(out, index)
}

func writeReportIndex(out io.Writer, index operator.ReportIndex) error {
	if _, err := fmt.Fprintf(out, "%d 份報告，其中 %d 份匯得出整份。\n",
		index.Total, index.Exportable); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "報告\t回答什麼\t範圍\t看得到多遠\t匯出"); err != nil {
		return err
	}
	for _, entry := range index.Entries {
		export := entry.ExportSentence
		if entry.ExportPath != "" {
			export = entry.ExportPath
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n",
			entry.Title, entry.Question, entry.RangeSentence, entry.HorizonSentence, export); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	for _, entry := range index.Entries {
		if _, err := fmt.Fprintf(out, "%s：%s　%s\n",
			entry.Title, entry.Path, entry.ScopeSentence); err != nil {
			return err
		}
	}
	return nil
}

// writeOperatorJSON 是每一支 operator 讀取指令共用的 JSON 輸出。
func writeOperatorJSON(out io.Writer, document any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(document)
}

func reportHTTPClient(label, explicitURL string, explicit bool, deps machineCommandDeps) (
	*operatorclient.Client, error,
) {
	if deps.newOperatorClient == nil {
		return nil, fmt.Errorf("%s: operator HTTP client 未初始化", label)
	}
	if explicit {
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("%s: 建立 HTTP operator client 失敗：%w", label, err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, fmt.Errorf("%s: Hub discovery 未初始化", label)
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("%s: 無法發現 Hub：%w", label, err)
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("%s: 建立 discovered HTTP operator client 失敗：%w", label, err)
	}
	return client, nil
}
