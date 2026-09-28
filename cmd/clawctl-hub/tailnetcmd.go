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
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func cmdTailnet(argv []string) {
	if err := runTailnetCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

type tailnetCommandDeps struct {
	machineCommandDeps
	newTailnetSource func() operator.TailnetSource
	now              func() time.Time
}

func productionTailnetCommandDeps() tailnetCommandDeps {
	return tailnetCommandDeps{
		machineCommandDeps: productionMachineCommandDeps(),
		newTailnetSource:   func() operator.TailnetSource { return tailnet.NewCache() },
		now:                time.Now,
	}
}

type tailnetCommandBackend interface {
	Tailnet(context.Context, time.Time) (operator.TailnetOverview, error)
	Preview(context.Context, operator.TailnetPeerIgnorePreviewRequest, time.Time) (operator.TailnetPeerIgnorePreview, error)
	Apply(context.Context, tailnetCommandApply, time.Time) (store.OperatorTailnetPeerIgnoreResult, error)
}

type tailnetHTTPBackend struct{ client *operatorclient.Client }

func (b tailnetHTTPBackend) Tailnet(ctx context.Context, _ time.Time) (operator.TailnetOverview, error) {
	return b.client.Tailnet(ctx)
}

func (b tailnetHTTPBackend) Preview(ctx context.Context, req operator.TailnetPeerIgnorePreviewRequest, _ time.Time) (operator.TailnetPeerIgnorePreview, error) {
	return b.client.PreviewTailnetPeerIgnore(ctx, req)
}

func (b tailnetHTTPBackend) Apply(ctx context.Context, req tailnetCommandApply, _ time.Time) (store.OperatorTailnetPeerIgnoreResult, error) {
	return b.client.PutTailnetPeerIgnore(ctx, req.PeerID, req.IdempotencyKey, operatorclient.TailnetPeerIgnoreRequest{
		Action: req.Action, ExpiresAt: req.ExpiresAt, ExpectedRevision: req.ExpectedRevision,
		ConfirmHostname: req.ConfirmHostname, PreviewDigest: req.PreviewDigest, Reason: req.Reason,
	})
}

type tailnetDirectBackend struct{ service *operator.Service }

func (b tailnetDirectBackend) Tailnet(ctx context.Context, now time.Time) (operator.TailnetOverview, error) {
	return b.service.Tailnet(ctx, now)
}

func (b tailnetDirectBackend) Preview(ctx context.Context, req operator.TailnetPeerIgnorePreviewRequest, now time.Time) (operator.TailnetPeerIgnorePreview, error) {
	return b.service.PreviewTailnetPeerIgnore(ctx, req, now)
}

func (b tailnetDirectBackend) Apply(ctx context.Context, req tailnetCommandApply, now time.Time) (store.OperatorTailnetPeerIgnoreResult, error) {
	return b.service.ApplyTailnetPeerIgnore(ctx, operator.TailnetPeerIgnoreApplyRequest{
		TailnetPeerIgnorePreviewRequest: operator.TailnetPeerIgnorePreviewRequest{
			PeerID: req.PeerID, Action: req.Action, ExpiresAt: req.ExpiresAt, Reason: req.Reason,
		},
		ExpectedRevision: req.ExpectedRevision, ConfirmHostname: req.ConfirmHostname,
		PreviewDigest: req.PreviewDigest, IdempotencyKey: req.IdempotencyKey,
		Actor: operator.Actor{
			SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub tailnet", SourceKind: operator.SourceKindDirectDBCLI,
		},
	}, now)
}

type tailnetCommandApply struct {
	PeerID           string
	Action           string
	ExpiresAt        time.Time
	ExpectedRevision int64
	ConfirmHostname  string
	PreviewDigest    string
	Reason           string
	IdempotencyKey   string
}

type tailnetCommandOptions struct {
	HubURL           string
	DBPath           string
	ExplicitHub      bool
	ExplicitDB       bool
	JSON             bool
	Preview          bool
	PeerID           string
	Action           string
	ExpiresAt        time.Time
	Reason           string
	ConfirmHostname  string
	IdempotencyKey   string
	PreviewDigest    string
	ExpectedRevision int64
	HasRetry         bool
}

func runTailnetCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runTailnetCommandWithDeps(ctx, argv, out, errOut, productionTailnetCommandDeps())
}

func runTailnetCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer, deps tailnetCommandDeps) error {
	if len(argv) > 0 && (argv[0] == "ignore" || argv[0] == "unignore") {
		return runTailnetMutationCommand(ctx, argv[0], argv[1:], out, errOut, deps)
	}
	fs := flag.NewFlagSet("tailnet", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub tailnet [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub tailnet ignore|unignore --peer-id ID --reason REASON [mutation flags]")
		fmt.Fprintln(errOut, "  預設使用 discovered HTTP operator API；--db 是 stopped-service direct DB break-glass。")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("tailnet: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	seen := visitedFlags(fs)
	options := tailnetCommandOptions{HubURL: *hubURL, DBPath: *dbPath, ExplicitHub: seen["hub-url"], ExplicitDB: seen["db"], JSON: *jsonOutput}
	return withTailnetCommandBackend(ctx, "tailnet read", options, deps, func(backend tailnetCommandBackend, source string, now time.Time) error {
		result, err := backend.Tailnet(ctx, now)
		if err != nil {
			return fmt.Errorf("讀取 Tailnet 清單失敗（%s）：%w", source, err)
		}
		if !result.Available {
			if options.JSON {
				if err := writeTailnetJSON(out, result); err != nil {
					return err
				}
			} else if err := writeTailnetOverview(out, source, result); err != nil {
				return err
			}
			return fmt.Errorf("Tailnet 清單無法取得（%s）：%s", source, result.Unavailable)
		}
		if options.JSON {
			return writeTailnetJSON(out, result)
		}
		return writeTailnetOverview(out, source, result)
	})
}

func runTailnetMutationCommand(ctx context.Context, action string, argv []string, out, errOut io.Writer, deps tailnetCommandDeps) error {
	fs := flag.NewFlagSet("tailnet "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintf(errOut, "用法：clawctl-hub tailnet %s --peer-id ID --reason REASON [--preview | --confirm-hostname HOST] [--json] [--hub-url URL | --db PATH]\n", action)
		fmt.Fprintln(errOut, "  ignore 預設 30 天；可用 --days 1..366，retry 則重用原 --expires-at。")
		fmt.Fprintln(errOut, "  ambiguous response retry：重用 --idempotency-key、--expected-revision、--preview-digest；ignore 另重用 --expires-at。")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	peerID := fs.String("peer-id", "", "Tailscale stable node ID")
	reason := fs.String("reason", "", "建立或移除規則的理由")
	confirm := fs.String("confirm-hostname", "", "套用時逐字確認 preview 顯示的 hostname")
	days := fs.Int("days", 30, "ignore 規則效期天數（1..366）")
	expiresRaw := fs.String("expires-at", "", "retry 使用原 RFC3339 expiry")
	previewOnly := fs.Bool("preview", false, "只顯示規則預覽，不套用")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	key := fs.String("idempotency-key", "", "ambiguous response retry 使用原 request key")
	revision := fs.Int64("expected-revision", 0, "ambiguous response retry 使用原 revision")
	digest := fs.String("preview-digest", "", "ambiguous response retry 使用原 preview digest")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("tailnet %s: 不接受 positional arguments：%q", action, strings.Join(fs.Args(), " "))
	}
	seen := visitedFlags(fs)
	if seen["hub-url"] && seen["db"] {
		return fmt.Errorf("tailnet %s: --hub-url 與 --db 不可同時明示", action)
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") || (seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return fmt.Errorf("tailnet %s: 明示的 --hub-url / --db 不可為空", action)
	}
	canonicalPeer, canonicalReason, canonicalConfirm := strings.TrimSpace(*peerID), strings.TrimSpace(*reason), strings.TrimSpace(*confirm)
	if canonicalPeer == "" || canonicalReason == "" {
		return fmt.Errorf("tailnet %s: --peer-id 與 --reason 必填", action)
	}
	retryCount := 0
	for _, name := range []string{"idempotency-key", "expected-revision", "preview-digest"} {
		if seen[name] {
			retryCount++
		}
	}
	if retryCount != 0 && retryCount != 3 {
		return fmt.Errorf("tailnet %s: retry 必須同時提供原 --idempotency-key、--expected-revision 與 --preview-digest", action)
	}
	if *previewOnly && (retryCount != 0 || seen["confirm-hostname"]) {
		return fmt.Errorf("tailnet %s: --preview 不接受 confirmation 或 apply retry coordinates", action)
	}
	if !*previewOnly && canonicalConfirm == "" {
		return fmt.Errorf("tailnet %s: apply 的 --confirm-hostname 必填，且不會由 preview 自動代填", action)
	}
	if retryCount == 3 && (strings.TrimSpace(*key) == "" || *revision < 0 || !validLifecycleRetryDigest(strings.TrimSpace(*digest))) {
		return fmt.Errorf("tailnet %s: retry key、revision 或 preview digest 不合法", action)
	}
	now := deps.now
	if now == nil {
		now = time.Now
	}
	expiresAt := time.Time{}
	if action == "ignore" {
		if *days < 1 || *days > 366 {
			return errors.New("tailnet ignore: --days 必須是 1 至 366")
		}
		if seen["expires-at"] {
			parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*expiresRaw))
			if err != nil {
				return fmt.Errorf("tailnet ignore: --expires-at 必須是 RFC3339：%w", err)
			}
			expiresAt = parsed.UTC()
		} else {
			if retryCount != 0 {
				return errors.New("tailnet ignore: retry 必須重用原 --expires-at")
			}
			expiresAt = now().UTC().Add(time.Duration(*days) * 24 * time.Hour)
		}
	} else if seen["days"] || seen["expires-at"] {
		return errors.New("tailnet unignore 不接受 --days 或 --expires-at")
	}
	options := tailnetCommandOptions{
		HubURL: *hubURL, DBPath: *dbPath, ExplicitHub: seen["hub-url"], ExplicitDB: seen["db"],
		JSON: *jsonOutput, Preview: *previewOnly, PeerID: canonicalPeer, Action: action,
		ExpiresAt: expiresAt, Reason: canonicalReason, ConfirmHostname: canonicalConfirm,
		IdempotencyKey: strings.TrimSpace(*key), PreviewDigest: strings.TrimSpace(*digest),
		ExpectedRevision: *revision, HasRetry: retryCount == 3,
	}
	return withTailnetCommandBackend(ctx, "tailnet "+action, options, deps, func(backend tailnetCommandBackend, source string, commandNow time.Time) error {
		return executeTailnetMutation(ctx, backend, source, options, commandNow, out, errOut)
	})
}

func withTailnetCommandBackend(ctx context.Context, command string, options tailnetCommandOptions, deps tailnetCommandDeps,
	run func(tailnetCommandBackend, string, time.Time) error,
) error {
	if options.ExplicitHub && options.ExplicitDB {
		return fmt.Errorf("%s: --hub-url 與 --db 不可同時明示", command)
	}
	nowFn := deps.now
	if nowFn == nil {
		nowFn = time.Now
	}
	if options.ExplicitDB {
		if deps.newTailnetSource == nil {
			return fmt.Errorf("%s: Tailnet source 未初始化", command)
		}
		return withDirectOperatorStore(ctx, command, options.DBPath, deps.machineCommandDeps, func(st *store.Store) error {
			return run(tailnetDirectBackend{service: operator.NewWithTailnet(st, deps.newTailnetSource())}, "direct DB operator service", nowFn().UTC())
		})
	}
	client, err := machinesHTTPClient(options.HubURL, options.ExplicitHub, deps.machineCommandDeps)
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return run(tailnetHTTPBackend{client: client}, "HTTP operator API", nowFn().UTC())
}

func executeTailnetMutation(ctx context.Context, backend tailnetCommandBackend, source string,
	options tailnetCommandOptions, now time.Time, out, errOut io.Writer,
) error {
	apply := tailnetCommandApply{
		PeerID: options.PeerID, Action: options.Action, ExpiresAt: options.ExpiresAt,
		ConfirmHostname: options.ConfirmHostname, Reason: options.Reason,
		ExpectedRevision: options.ExpectedRevision, PreviewDigest: options.PreviewDigest,
		IdempotencyKey: options.IdempotencyKey,
	}
	if !options.HasRetry {
		preview, err := backend.Preview(ctx, operator.TailnetPeerIgnorePreviewRequest{
			PeerID: options.PeerID, Action: options.Action, ExpiresAt: options.ExpiresAt, Reason: options.Reason,
		}, now)
		if err != nil {
			return fmt.Errorf("預覽 Tailnet 規則失敗（%s）：%w", source, err)
		}
		if options.Preview {
			if options.JSON {
				return writeTailnetJSON(out, preview)
			}
			return writeTailnetPreview(out, source, preview)
		}
		key, err := operator.NewIdempotencyKey("cli-tailnet-peer-ignore")
		if err != nil {
			return err
		}
		apply.ExpectedRevision, apply.PreviewDigest, apply.IdempotencyKey = preview.ExpectedRevision, preview.PreviewDigest, key
	}
	if !apply.ExpiresAt.IsZero() {
		fmt.Fprintf(errOut, "tailnet private retry coordinates: idempotency-key=%s expected-revision=%d preview-digest=%s expires-at=%s\n",
			apply.IdempotencyKey, apply.ExpectedRevision, apply.PreviewDigest, apply.ExpiresAt.Format(time.RFC3339Nano))
	} else {
		fmt.Fprintf(errOut, "tailnet private retry coordinates: idempotency-key=%s expected-revision=%d preview-digest=%s\n",
			apply.IdempotencyKey, apply.ExpectedRevision, apply.PreviewDigest)
	}
	result, err := backend.Apply(ctx, apply, now)
	if err != nil {
		if lifecycleReplayedError(err) {
			if current, readErr := backend.Tailnet(ctx, now); readErr == nil && current.Available {
				_ = writeTailnetOverview(errOut, source+" authoritative current", current)
			}
		}
		return fmt.Errorf("套用 Tailnet 規則失敗（%s；idempotency-key=%q expected-revision=%d%s）：%w",
			source, apply.IdempotencyKey, apply.ExpectedRevision, operatorRejectionReplayNote(err), err)
	}
	var current *operator.TailnetOverview
	if result.Replayed {
		read, err := backend.Tailnet(ctx, now)
		if err != nil || !read.Available {
			return fmt.Errorf("Tailnet 規則已回放歷史 receipt，但重新讀取 authoritative current 失敗：%v", err)
		}
		current = &read
	}
	if options.JSON {
		return writeTailnetJSON(out, struct {
			store.OperatorTailnetPeerIgnoreResult
			AuthoritativeCurrent *operator.TailnetOverview `json:"authoritative_current,omitempty"`
		}{OperatorTailnetPeerIgnoreResult: result, AuthoritativeCurrent: current})
	}
	if err := writeTailnetApply(out, source, result, apply.IdempotencyKey); err != nil {
		return err
	}
	if current != nil {
		return writeTailnetOverview(out, source+" authoritative current", *current)
	}
	return nil
}

func visitedFlags(fs *flag.FlagSet) map[string]bool {
	seen := map[string]bool{}
	fs.Visit(func(item *flag.Flag) { seen[item.Name] = true })
	return seen
}

func writeTailnetJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeTailnetOverview(out io.Writer, source string, result operator.TailnetOverview) error {
	if !result.Available {
		if _, err := fmt.Fprintf(out, "%s: unavailable=%s local-ignore-rules=%d legacy-ignore-rules=%d\n",
			source, terminalSafe(result.Unavailable), len(result.Ignored), result.LegacyIgnoredCount); err != nil {
			return err
		}
		return writeTailnetIgnoreRules(out, result.Ignored)
	}
	if _, err := fmt.Fprintf(out, "%s: observed=%s peers=%d unenrolled=%d ignored=%d online-but-silent=%d retired-but-online=%d\n",
		source, result.ObservedAt.Format(time.RFC3339), result.PeerCount, len(result.Unenrolled), len(result.Ignored),
		len(result.OnlineButSilent), len(result.RetiredButOnline)); err != nil {
		return err
	}
	for _, peer := range result.Unenrolled {
		status := "offline"
		if peer.Online {
			status = "online"
		}
		if _, err := fmt.Fprintf(out, "unenrolled peer-id=%s hostname=%s ip=%s os=%s status=%s\n",
			terminalSafe(peer.StableID), terminalSafe(peer.Hostname), terminalSafe(peer.IP), terminalSafe(peer.OS), status); err != nil {
			return err
		}
	}
	return writeTailnetIgnoreRules(out, result.Ignored)
}

func writeTailnetIgnoreRules(out io.Writer, rules []store.TailnetPeerIgnore) error {
	for _, item := range rules {
		if _, err := fmt.Fprintf(out, "ignored peer-id=%s hostname=%s revision=%d expires-at=%s reason=%s\n",
			terminalSafe(item.PeerID), terminalSafe(item.Hostname), item.Revision, item.ExpiresAt.Format(time.RFC3339), terminalSafe(item.Reason)); err != nil {
			return err
		}
	}
	return nil
}

func writeTailnetPreview(out io.Writer, source string, preview operator.TailnetPeerIgnorePreview) error {
	expiry := "none"
	if !preview.ExpiresAt.IsZero() {
		expiry = preview.ExpiresAt.Format(time.RFC3339Nano)
	}
	_, err := fmt.Fprintf(out, "%s preview: peer-id=%s hostname=%s action=%s ignored=%t→%t expected-revision=%d expires-at=%s reason=%s\npreview-digest=%s\n",
		source, terminalSafe(preview.PeerID), terminalSafe(preview.Hostname), preview.Action,
		preview.CurrentlyIgnored, preview.IgnoredAfter, preview.ExpectedRevision, expiry,
		terminalSafe(preview.Reason), preview.PreviewDigest)
	return err
}

func writeTailnetApply(out io.Writer, source string, result store.OperatorTailnetPeerIgnoreResult, key string) error {
	expiry := "none"
	if !result.ExpiresAt.IsZero() {
		expiry = result.ExpiresAt.Format(time.RFC3339Nano)
	}
	_, err := fmt.Fprintf(out, "%s: peer-id=%s hostname=%s action=%s ignored=%t→%t revision=%d expires-at=%s replayed=%t idempotency-key=%s\n",
		source, terminalSafe(result.PeerID), terminalSafe(result.Hostname), result.Action,
		result.PreviousIgnored, result.Ignored, result.Revision, expiry, result.Replayed, terminalSafe(key))
	return err
}
