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
		fmt.Fprintln(errOut, "用法：clawctl-hub report [--since DURATION] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  顯示現在會產生的每日早報；不送出，也不寫推播紀錄。")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fs.PrintDefaults()
	}
	var hubURL, dbPath auditStringFlag
	since := auditStringFlag{value: operator.DefaultDailyReportWindow.String()}
	listen := auditStringFlag{value: os.Getenv("CLAWCTL_LISTEN")}
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&dbPath, "db", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	fs.Var(&since, "since", "跟多久以前比（整秒，1s..720h）")
	fs.Var(&listen, "listen", "direct mode 早報連結使用的 Hub 監聽位址（預設讀 $CLAWCTL_LISTEN）")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("report: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set && dbPath.set {
		return errors.New("report: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if listen.set && !dbPath.set {
		return errors.New("report: --listen 只適用於明示 --db 的 stopped-service direct mode")
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
				return fmt.Errorf("report: --%s 不可為空、含首尾空白或控制字元", field.name)
			}
		}
	}
	window, err := time.ParseDuration(since.value)
	if err != nil || window%time.Second != 0 || window < time.Second || window > operator.MaxDailyReportWindow {
		return fmt.Errorf("report: --since 必須是 1s 到 %s 的整秒 duration", operator.MaxDailyReportWindow)
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
		return fmt.Errorf("讀取每日早報失敗（HTTP operator API）：%w", err)
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
			return fmt.Errorf("report: shell CLAWCTL_EXPECTATIONS 與 Hub 最後發布的 workload policy 不一致；拒絕 direct DB 判決：%w", err)
		}
		h := &hub{store: st, drillStamp: drillStampPath(dbPath)}
		var why string
		if h.publicURL, why = publicBase(listen); why != "" {
			fmt.Fprintf(errOut, "（早報不會帶連結：%s）\n", why)
		}
		result, err := h.dailyReportAt(time.Now().UTC().Truncate(time.Second), window)
		if err != nil {
			return fmt.Errorf("產生每日早報失敗（direct DB）：%w", err)
		}
		_, err = io.WriteString(out, result.Body)
		return err
	})
}
