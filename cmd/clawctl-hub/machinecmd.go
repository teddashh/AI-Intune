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

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func cmdMachine(argv []string) {
	if err := runMachineCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runMachineCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runMachineCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

type machineCommandDeps struct {
	discoverHubURL       func() (string, error)
	newOperatorClient    func(string) (*operatorclient.Client, error)
	validateDirectPath   func(string) error
	validateDirectLedger func(string) error
	acquireDirect        func(string) (io.Closer, error)
	checkMaintenance     func(string) error
	verifyHubStopped     func(context.Context, string) error
	openDirectDB         func(string) (*store.Store, error)
}

func productionMachineCommandDeps() machineCommandDeps {
	return machineCommandDeps{
		discoverHubURL:       discoverOperatorHubURL,
		newOperatorClient:    operatorclient.New,
		validateDirectPath:   ledgerlock.ValidateExistingDB,
		validateDirectLedger: store.ValidateExistingLedger,
		acquireDirect: func(path string) (io.Closer, error) {
			return ledgerlock.AcquireDirect(path)
		},
		checkMaintenance: rejectDBWhileUpgradeMaintenance,
		verifyHubStopped: verifyManagedHubStopped,
		openDirectDB:     openExisting,
	}
}

func runMachineCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer, deps machineCommandDeps) error {
	printUsage := func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub machine channel --machine <name|id> --set canary|stable|none --confirm-name <display_name> [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub machine lifecycle --machine <name|id> [--set active|retired (--preview | --reason REASON --confirm-name NAME)] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub machine actions --machine <id> [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "      clawctl-hub machine rename --machine <id> --set <display_name> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "      clawctl-hub machine notes --machine <id> --set <notes> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "      clawctl-hub machine timeline --machine <id> [--days 1..30] [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "      clawctl-hub machine data --machine <id> [--json | --csv] [--hub-url URL]")
	}
	if len(argv) == 0 {
		printUsage()
		return errors.New("machine: 必須指定 channel、lifecycle、rename、notes、actions、timeline 或 data subcommand")
	}
	if argv[0] == "-h" || argv[0] == "--help" {
		printUsage()
		return flag.ErrHelp
	}
	if argv[0] == "lifecycle" {
		return runMachineLifecycleSubcommand(ctx, argv[1:], out, errOut, deps)
	}
	if argv[0] == "actions" {
		return runMachineActionsSubcommand(ctx, argv[1:], out, errOut, deps)
	}
	if argv[0] == "rename" {
		return runMachineRenameSubcommand(ctx, argv[1:], out, errOut, deps)
	}
	if argv[0] == "notes" {
		return runMachineNotesSubcommand(ctx, argv[1:], out, errOut, deps)
	}
	if argv[0] == "data" {
		return runMachineDataSubcommand(ctx, argv[1:], out, errOut, deps)
	}
	if argv[0] == "timeline" {
		return runMachineTimelineSubcommand(ctx, argv[1:], out, errOut, deps)
	}
	if argv[0] != "channel" {
		printUsage()
		return fmt.Errorf("machine: 不認得 subcommand %q", argv[0])
	}
	fs := flag.NewFlagSet("machine channel", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub machine channel --machine <name|id> --set canary|stable|none --confirm-name <display_name> [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API：--hub-url 優先，否則讀 CLAWCTL_HUB_URL 或 $XDG_CONFIG_HOME/clawctl/operator.json；--machine 必須是 machine_id。")
		fmt.Fprintln(errOut, "  --db 是明示的 stopped-service direct DB break-glass；要求既有 canonical ledger、upgrade+writer locks，且受控 Hub unit 必須釘住同一 DB 並完全停止。")
		fmt.Fprintln(errOut, "  ambiguous response 後重試時，必須把原本的 --idempotency-key 與 --expected-revision 一起重用。")
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	machine := fs.String("machine", "", "direct DB: display_name 或 machine_id；HTTP: machine_id")
	set := fs.String("set", "", "canary、stable 或 none")
	confirmName := fs.String("confirm-name", "", "操作者預期的 machine display_name（必填，不從 GET 自動代填）")
	idempotencyKey := fs.String("idempotency-key", "", "明示 request key；重試時與 --expected-revision 一起重用")
	expectedRevision := fs.Int64("expected-revision", 0, "明示原 request 的 channel revision；須與 --idempotency-key 一起使用")
	if err := fs.Parse(argv[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine channel: 不接受多餘的 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("machine channel: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if strings.TrimSpace(*confirmName) == "" {
		return errors.New("machine channel: --confirm-name 必填；必須由操作者明示預期的 display_name")
	}
	if seen["idempotency-key"] != seen["expected-revision"] {
		return errors.New("machine channel: --idempotency-key 與 --expected-revision 必須一起提供，才能保留同一個 canonical request")
	}
	if seen["idempotency-key"] && strings.TrimSpace(*idempotencyKey) == "" {
		return errors.New("machine channel: --idempotency-key 不可為空")
	}
	inputs := machineChannelInputs{
		Machine: *machine, Set: *set, ConfirmName: *confirmName,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	}
	if seen["expected-revision"] {
		inputs.ExpectedRevision = expectedRevision
	}
	if seen["hub-url"] {
		if strings.TrimSpace(*hubURL) == "" {
			return errors.New("machine channel: --hub-url 不可為空")
		}
		if deps.newOperatorClient == nil {
			return errors.New("machine channel: operator HTTP client 未初始化")
		}
		client, err := deps.newOperatorClient(*hubURL)
		if err != nil {
			return fmt.Errorf("設定 channel 失敗（HTTP operator API）：%w", err)
		}
		return runMachineChannelHTTP(ctx, client, inputs, out)
	}
	if seen["db"] {
		if strings.TrimSpace(*dbPath) == "" {
			return errors.New("machine channel: --db 不可為空")
		}
		return runMachineChannelDirect(ctx, *dbPath, inputs, out, deps)
	}
	if deps.discoverHubURL == nil {
		return errors.New("設定 channel 失敗：Hub discovery 未初始化")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return fmt.Errorf("設定 channel 失敗：無法發現 Hub：%w", err)
	}
	if deps.newOperatorClient == nil {
		return errors.New("設定 channel 失敗：operator HTTP client 未初始化")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return fmt.Errorf("設定 channel 失敗（discovered HTTP operator API）：%w", err)
	}
	return runMachineChannelHTTP(ctx, client, inputs, out)
}

func runMachineChannelDirect(ctx context.Context, dbPath string, inputs machineChannelInputs, out io.Writer, deps machineCommandDeps) (retErr error) {
	return withDirectOperatorStore(ctx, "machine channel", dbPath, deps, func(st *store.Store) error {
		return runMachineChannel(st, inputs, out)
	})
}

// withDirectOperatorStore is the single stopped-service break-glass gate for
// operator commands that still support an explicit --db fallback. Normal CLI
// traffic never reaches this function; it uses the authenticated HTTP API.
// Keeping the lock, ledger-identity and exact-systemd proof in one helper makes
// it harder for the next vertical slice to accidentally grow a weaker second
// writer path.
func withDirectOperatorStore(ctx context.Context, command, dbPath string, deps machineCommandDeps,
	run func(*store.Store) error,
) (retErr error) {
	if deps.validateDirectPath == nil || deps.validateDirectLedger == nil || deps.acquireDirect == nil || deps.checkMaintenance == nil ||
		deps.verifyHubStopped == nil || deps.openDirectDB == nil || run == nil {
		return fmt.Errorf("%s: direct DB safety dependencies 未初始化；拒絕 break-glass", command)
	}
	// Only validate the pathname before locking. Even a read-only SQLite open
	// may recreate a missing -shm file for a live WAL database, so ledger
	// content inspection belongs strictly inside both process locks.
	if err := deps.validateDirectPath(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target pre-open 拒絕：%w", command, err)
	}
	guard, err := deps.acquireDirect(dbPath)
	if err != nil {
		return fmt.Errorf("%s: upgrade→writer break-glass lock 取得失敗（pre-open）：%w", command, err)
	}
	defer func() {
		if err := guard.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("釋放 direct DB locks：%w", err))
		}
	}()

	if err := deps.validateDirectPath(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target 在取得 locks 後改變；拒絕開啟 SQLite：%w", command, err)
	}
	if err := deps.validateDirectLedger(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target 不是可遷移的 clawctl ledger（已取得並將釋放 locks）：%w", command, err)
	}
	if err := deps.checkMaintenance(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB break-glass 拒絕：%w", command, err)
	}
	if err := deps.verifyHubStopped(ctx, dbPath); err != nil {
		return fmt.Errorf("%s: direct DB break-glass 拒絕：%w", command, err)
	}
	// systemd proof may take several seconds. Recheck the pathname immediately
	// before SQLite sees it rather than trusting the pre-query identity.
	if err := deps.validateDirectPath(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target 在 stopped proof 後改變；拒絕開啟 SQLite：%w", command, err)
	}
	if err := deps.validateDirectLedger(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB ledger 在 stopped proof 後改變；拒絕可寫開啟：%w", command, err)
	}
	st, err := deps.openDirectDB(dbPath)
	if err != nil {
		return fmt.Errorf("開啟 direct DB %s 失敗：%w", dbPath, err)
	}
	if err := deps.validateDirectPath(dbPath); err != nil {
		return errors.Join(fmt.Errorf("%s: direct DB path 在 writable open 後改變：%w", command, err), st.Close())
	}
	if err := deps.validateDirectLedger(dbPath); err != nil {
		return errors.Join(fmt.Errorf("%s: direct DB ledger 在 migration 後 identity 拒絕：%w", command, err), st.Close())
	}
	defer func() {
		if err := st.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("關閉 direct DB：%w", err))
		}
	}()
	return run(st)
}

type machineChannelInputs struct {
	Machine          string
	Set              string
	ConfirmName      string
	IdempotencyKey   string
	ExpectedRevision *int64
}

const replayedRejectionNote = "；idempotency replay：這是原判決，沒有依目前狀態重新評估"

func operatorRejectionReplayNote(err error) string {
	var apiErr *operatorclient.APIError
	if errors.As(err, &apiErr) && apiErr.Replayed {
		return replayedRejectionNote
	}
	var requestErr *store.OperatorRequestError
	if errors.As(err, &requestErr) && requestErr.Replayed {
		return replayedRejectionNote
	}
	return ""
}

func operatorRequestKey(supplied string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	return operator.NewIdempotencyKey("cli-machine-channel")
}

func runMachineChannel(st *store.Store, inputs machineChannelInputs, out io.Writer) error {
	// Resolve retired machines too, then let the shared operator service reject
	// them with the same persisted verdict/audit as HTTP and Web.
	m, err := resolveMachine(st, inputs.Machine, true)
	if err != nil {
		return fmt.Errorf("設定 channel 失敗：%w", err)
	}
	key, err := operatorRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("設定 channel 失敗：%w", err)
	}
	expected := m.ChannelRevision
	if inputs.ExpectedRevision != nil {
		expected = *inputs.ExpectedRevision
	}
	result, err := operator.New(st).ChangeMachineChannel(operator.MachineChannelRequest{
		MachineID: m.MachineID, Channel: inputs.Set, ExpectedRevision: &expected,
		ConfirmDisplayName: inputs.ConfirmName, IdempotencyKey: key,
		Actor: operator.Actor{
			SourceAddr:     "local-cli",
			WhoUnavailable: "direct-db-cli",
			UserAgent:      "clawctl-hub machine channel",
			SourceKind:     operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("設定 channel 失敗（idempotency-key=%q expected-revision=%d%s）：%w",
			key, expected, operatorRejectionReplayNote(err), err)
	}
	before, after := result.PreviousChannel, result.Channel
	if before == "" {
		before = "none"
	}
	if after == "" {
		after = "none"
	}
	replay := ""
	if result.Replayed {
		replay = "；idempotency replay"
	}
	_, err = fmt.Fprintf(out, "direct DB operator service: %s (%s): %s → %s；revision=%d；idempotency-key=%s%s\n",
		m.DisplayName, m.MachineID, before, after, result.Revision, key, replay)
	return err
}

func runMachineChannelHTTP(ctx context.Context, client *operatorclient.Client, inputs machineChannelInputs, out io.Writer) error {
	if strings.TrimSpace(inputs.Machine) == "" {
		return errors.New("設定 channel 失敗（HTTP operator API）：--machine 必須是 machine_id")
	}
	current, err := client.GetMachineChannel(ctx, inputs.Machine)
	if err != nil {
		return fmt.Errorf("設定 channel 失敗（HTTP operator API；--machine 必須是 machine_id）：%w", err)
	}
	key, err := operatorRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("設定 channel 失敗（HTTP operator API）：%w", err)
	}
	expected := current.Revision
	if inputs.ExpectedRevision != nil {
		expected = *inputs.ExpectedRevision
	}
	result, err := client.PutMachineChannel(ctx, inputs.Machine, key, operatorclient.MachineChannelRequest{
		Channel: inputs.Set, ExpectedRevision: expected, ConfirmDisplayName: inputs.ConfirmName,
	})
	if err != nil {
		return fmt.Errorf("設定 channel 失敗（HTTP operator API；idempotency-key=%q expected-revision=%d%s）：%w",
			key, expected, operatorRejectionReplayNote(err), err)
	}
	before, after := result.PreviousChannel, result.Channel
	if before == "" {
		before = "none"
	}
	if after == "" {
		after = "none"
	}
	replay := ""
	if result.Replayed {
		replay = "；idempotency replay"
	}
	_, err = fmt.Fprintf(out, "HTTP operator API: %s (%s): %s → %s；revision=%d；ETag=%s；idempotency-key=%s%s\n",
		result.DisplayName, result.MachineID, before, after, result.Revision, result.Meta.ETag, key, replay)
	return err
}
