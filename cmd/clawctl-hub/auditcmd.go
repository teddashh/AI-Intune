package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type repeatedAuditActions []string

func (v *repeatedAuditActions) String() string { return strings.Join(*v, ",") }
func (v *repeatedAuditActions) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type auditStringFlag struct {
	value string
	set   bool
}

func (v *auditStringFlag) String() string { return v.value }
func (v *auditStringFlag) Set(value string) error {
	if v.set {
		return errors.New("不可重複")
	}
	v.value, v.set = value, true
	return nil
}

type auditIntFlag struct {
	value int
	set   bool
}

func (v *auditIntFlag) String() string { return strconv.Itoa(v.value) }
func (v *auditIntFlag) Set(value string) error {
	if v.set {
		return errors.New("不可重複")
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return errors.New("必須是十進位整數")
	}
	v.value, v.set = parsed, true
	return nil
}

type auditBoolFlag struct {
	value bool
	set   bool
}

func (v *auditBoolFlag) String() string   { return strconv.FormatBool(v.value) }
func (v *auditBoolFlag) IsBoolFlag() bool { return true }
func (v *auditBoolFlag) Set(value string) error {
	if v.set {
		return errors.New("不可重複")
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return errors.New("必須是 boolean")
	}
	v.value, v.set = parsed, true
	return nil
}

func runAuditCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runAuditCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runAuditCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if len(argv) > 0 && argv[0] == "list" {
		argv = argv[1:]
	}
	fs := flag.NewFlagSet("audit list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub audit [list] [filters] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fmt.Fprintln(errOut, "  預設只納入最新 50 筆 operator boundary denials，避免拒絕噪音遮住控制動作；--denials all 可停用抽樣。")
		fs.PrintDefaults()
	}
	var hubURL, dbPath auditStringFlag
	var machine, outcome, principal, capability, sourceKind, correlation auditStringFlag
	var from, to, denials, cursor auditStringFlag
	limit := auditIntFlag{value: operator.DefaultAuditReadLimit}
	var jsonOutput auditBoolFlag
	var actionValues repeatedAuditActions
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&dbPath, "db", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	fs.Var(&machine, "machine", "只看這個 machine_id（direct DB 也接受 display_name）")
	fs.Var(&actionValues, "action", "只看這個 canonical audit action；可重複")
	fs.Var(&outcome, "outcome", "只看 ok 或 failed")
	fs.Var(&principal, "principal", "精確比對 auth_subject 或 who_user")
	fs.Var(&capability, "capability", "精確比對 verified capability")
	fs.Var(&sourceKind, "source-kind", "精確比對 source_kind")
	fs.Var(&correlation, "correlation", "精確比對 idempotency key 或 request digest")
	fs.Var(&from, "from", "起始時間（second-precision RFC3339，含端點）")
	fs.Var(&to, "to", "結束時間（second-precision RFC3339，含端點）")
	fs.Var(&denials, "denials", "operator denials：sampled 或 all（預設 sampled）")
	fs.Var(&limit, "limit", "每頁最多幾筆（1..100）")
	fs.Var(&cursor, "cursor", "上一頁回傳的 opaque next cursor")
	fs.Var(&jsonOutput, "json", "輸出 stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("audit list: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set && dbPath.set {
		return errors.New("audit list: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	for _, field := range []struct {
		name  string
		value auditStringFlag
		max   int
	}{
		{"hub-url", hubURL, 2048}, {"db", dbPath, 4096}, {"machine", machine, 256},
		{"outcome", outcome, 16}, {"principal", principal, 512}, {"capability", capability, 512},
		{"source-kind", sourceKind, 128}, {"correlation", correlation, 256},
		{"from", from, 64}, {"to", to, 64}, {"denials", denials, 16}, {"cursor", cursor, 2048},
	} {
		if field.value.set {
			if err := validateJobReadCLIValue(field.name, field.value.value, field.max); err != nil {
				return fmt.Errorf("audit list: %w", err)
			}
		}
	}
	if limit.value < 1 || limit.value > operator.MaxAuditReadLimit {
		return fmt.Errorf("audit list: --limit 必須介於 1 與 %d", operator.MaxAuditReadLimit)
	}

	request := operator.AuditListRequest{
		MachineID: machine.value, Principal: principal.value, Capability: capability.value,
		SourceKind: sourceKind.value, Correlation: correlation.value, Limit: limit.value,
		Cursor: cursor.value,
	}
	seenActions := make(map[store.AuditAction]bool, len(actionValues))
	for _, value := range actionValues {
		action := store.AuditAction(value)
		if !store.IsKnownAuditAction(action) || seenActions[action] {
			return fmt.Errorf("audit list: --action %q 不是 canonical action 或重複", value)
		}
		seenActions[action] = true
		request.Actions = append(request.Actions, action)
	}
	if outcome.value != "" {
		request.Outcome = store.AuditOutcome(outcome.value)
		if request.Outcome != store.AuditOutcomeOK && request.Outcome != store.AuditOutcomeFailed {
			return errors.New("audit list: --outcome 必須是 ok 或 failed")
		}
	}
	if denials.value != "" {
		request.Denials = operator.AuditDenialMode(denials.value)
		if request.Denials != operator.AuditDenialsSampled && request.Denials != operator.AuditDenialsAll {
			return errors.New("audit list: --denials 必須是 sampled 或 all")
		}
	}
	for _, field := range []struct {
		name  string
		value auditStringFlag
		set   func(*time.Time)
	}{
		{"from", from, func(value *time.Time) { request.From = value }},
		{"to", to, func(value *time.Time) { request.To = value }},
	} {
		if !field.value.set {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, field.value.value)
		if err != nil || parsed.Nanosecond() != 0 {
			return fmt.Errorf("audit list: --%s 必須是 second-precision RFC3339", field.name)
		}
		parsed = parsed.UTC()
		field.set(&parsed)
	}
	if request.From != nil && request.To != nil && request.From.After(*request.To) {
		return errors.New("audit list: --from 不可晚於 --to")
	}

	if dbPath.set {
		return runAuditDirect(ctx, request, dbPath.value, jsonOutput.value, out, deps)
	}
	client, err := auditHTTPClient(hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	result, err := client.AuditEvents(ctx, request)
	if err != nil {
		return fmt.Errorf("讀取 audit list 失敗（HTTP operator API）：%w", err)
	}
	return writeAuditList(out, result, jsonOutput.value, "HTTP operator API")
}

func auditHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, errors.New("audit list: operator HTTP client 未初始化")
		}
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("audit list: 建立 HTTP operator client 失敗：%w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("audit list: Hub discovery 未初始化")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("audit list: 無法發現 Hub：%w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, errors.New("audit list: operator HTTP client 未初始化")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("audit list: 建立 discovered HTTP operator client 失敗：%w", err)
	}
	return client, nil
}

func runAuditDirect(ctx context.Context, request operator.AuditListRequest, dbPath string,
	jsonOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "audit list", dbPath, deps, func(st *store.Store) error {
		if request.MachineID != "" {
			machine, err := resolveMachine(st, request.MachineID, true)
			if err != nil {
				return err
			}
			request.MachineID = machine.MachineID
		}
		result, err := operator.New(st).ListAuditContext(ctx, request, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("讀取 audit list 失敗（direct DB operator service）：%w", err)
		}
		return writeAuditList(out, result, jsonOutput, "direct DB operator service")
	})
}

func writeAuditList(out io.Writer, result operator.AuditListResult, jsonOutput bool, source string) error {
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	if _, err := fmt.Fprintf(out,
		"%s；Hub 評估時間 %s；consistency %s；依 writer sequence 的固定 creation ceiling 分頁。\n"+
			"可見 %d / 採樣前符合 %d 筆（成功 %d、失敗 %d、結果無法判讀 %d）。\n"+
			"operator denials：%s，符合 %d、納入 %d、省略 %d。\n",
		source, result.EvaluatedAt.Format(time.RFC3339Nano), result.Consistency, result.Total, result.MatchedTotal,
		result.Succeeded, result.Failed, result.UnknownOutcome, result.Denials.Mode,
		result.Denials.Matched, result.Denials.Included, result.Denials.Omitted); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		_, err := fmt.Fprintln(out, "沒有符合 filter 的稽核事件。")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SEQ\tAT\tACTION\tOUTCOME\tSUBJECT\tPRINCIPAL / SOURCE\tCORRELATION"); err != nil {
		return err
	}
	for _, event := range result.Items {
		at := "unknown (invalid ledger time)"
		if event.At != nil {
			at = event.At.UTC().Format(time.RFC3339)
		}
		outcome := "unknown"
		if event.Outcome != nil {
			outcome = string(*event.Outcome)
		}
		// Keep this wording aligned with internal/web/templates/audit.html;
		// tests must catch drift between the two presentations.
		subject := "對象無法判讀"
		if event.Subject != "" {
			subject = terminalSafe(event.Subject)
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			event.AuditID, at, terminalSafe(event.Action), outcome, subject,
			terminalSafe(auditEventPrincipal(event)), terminalSafe(auditEventCorrelation(event))); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if result.NextCursor != nil {
		_, err := fmt.Fprintf(out, "next cursor: %s\n", terminalSafe(*result.NextCursor))
		return err
	}
	return nil
}

func auditEventPrincipal(event operator.AuditEvent) string {
	if event.AuthSubject != nil {
		return *event.AuthSubject
	}
	if event.WhoUser != nil {
		return *event.WhoUser
	}
	return event.WhoLabel()
}

func auditEventCorrelation(event operator.AuditEvent) string {
	if event.IdempotencyKey != nil {
		return *event.IdempotencyKey
	}
	if event.RequestDigest != nil {
		return *event.RequestDigest
	}
	return "-"
}
