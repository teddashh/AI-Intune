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
	"github.com/teddashh/AI-Intune/internal/tailnet"
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
	tailnetSource        operator.TailnetSource
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
		tailnetSource:    tailnet.NewCache(),
	}
}

func runMachineCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer, deps machineCommandDeps) error {
	printUsage := func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine assigned-user --machine <name|id> --user <user_id|none> --confirm-name <display_name> [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine channel --machine <name|id> --set canary|stable|none --confirm-name <display_name> [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub machine lifecycle --machine <name|id> [--set active|retired (--preview | --reason REASON --confirm-name NAME)] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub machine actions --machine <id> [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "       clawctl-hub machine rename --machine <id> --set <display_name> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "       clawctl-hub machine notes --machine <id> --set <notes> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "       clawctl-hub machine timeline --machine <id> [--days 1..30] [--json | --csv] [--hub-url URL]")
		fmt.Fprintln(errOut, "       clawctl-hub machine data --machine <id> [--json | --csv] [--hub-url URL]")
	}
	if len(argv) == 0 {
		printUsage()
		return errors.New("machine: must specify assigned-user, channel, lifecycle, rename, notes, actions, timeline, or data subcommand")
	}
	if argv[0] == "-h" || argv[0] == "--help" {
		printUsage()
		return flag.ErrHelp
	}
	if argv[0] == "assigned-user" {
		return runMachineAssignedUserSubcommand(ctx, argv[1:], out, errOut, deps)
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
		return fmt.Errorf("machine: unrecognized subcommand %q", argv[0])
	}
	fs := flag.NewFlagSet("machine channel", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine channel --machine <name|id> --set canary|stable|none --confirm-name <display_name> [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  Normal mode uses HTTP operator API: --hub-url takes precedence, otherwise reads CLAWCTL_HUB_URL or $XDG_CONFIG_HOME/clawctl/operator.json; --machine must be machine_id.")
		fmt.Fprintln(errOut, "  --db is an explicit stopped-service direct DB break-glass; requires an existing canonical ledger, upgrade+writer locks, and the managed Hub unit must be pinned to the same DB and completely stopped.")
		fmt.Fprintln(errOut, "  When retrying after an ambiguous response, original --idempotency-key and --expected-revision must be reused together.")
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "Existing SQLite file path for stopped-service direct DB break-glass")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered if omitted)")
	machine := fs.String("machine", "", "direct DB: display_name or machine_id; HTTP: machine_id")
	set := fs.String("set", "", "canary, stable, or none")
	confirmName := fs.String("confirm-name", "", "Operator expected machine display_name (required, not auto-populated from GET)")
	idempotencyKey := fs.String("idempotency-key", "", "Explicit request key; reuse together with --expected-revision when retrying")
	expectedRevision := fs.Int64("expected-revision", 0, "Explicit channel revision of the original request; must be used with --idempotency-key")
	if err := fs.Parse(argv[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine channel: unexpected positional arguments: %q", strings.Join(fs.Args(), " "))
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("machine channel: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified")
	}
	if strings.TrimSpace(*confirmName) == "" {
		return errors.New("machine channel: --confirm-name is required; operator must explicitly provide expected display_name")
	}
	if seen["idempotency-key"] != seen["expected-revision"] {
		return errors.New("machine channel: --idempotency-key and --expected-revision must be provided together to preserve the same canonical request")
	}
	if seen["idempotency-key"] && strings.TrimSpace(*idempotencyKey) == "" {
		return errors.New("machine channel: --idempotency-key cannot be empty")
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
			return errors.New("machine channel: --hub-url cannot be empty")
		}
		if deps.newOperatorClient == nil {
			return errors.New("machine channel: operator HTTP client not initialized")
		}
		client, err := deps.newOperatorClient(*hubURL)
		if err != nil {
			return fmt.Errorf("set channel failed (HTTP operator API): %w", err)
		}
		return runMachineChannelHTTP(ctx, client, inputs, out)
	}
	if seen["db"] {
		if strings.TrimSpace(*dbPath) == "" {
			return errors.New("machine channel: --db cannot be empty")
		}
		return runMachineChannelDirect(ctx, *dbPath, inputs, out, deps)
	}
	if deps.discoverHubURL == nil {
		return errors.New("set channel failed: Hub discovery not initialized")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return fmt.Errorf("set channel failed: unable to discover Hub: %w", err)
	}
	if deps.newOperatorClient == nil {
		return errors.New("set channel failed: operator HTTP client not initialized")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return fmt.Errorf("set channel failed (discovered HTTP operator API): %w", err)
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
		return fmt.Errorf("%s: direct DB safety dependencies not initialized; break-glass rejected", command)
	}
	// Only validate the pathname before locking. Even a read-only SQLite open
	// may recreate a missing -shm file for a live WAL database, so ledger
	// content inspection belongs strictly inside both process locks.
	if err := deps.validateDirectPath(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target pre-open rejected: %w", command, err)
	}
	guard, err := deps.acquireDirect(dbPath)
	if err != nil {
		return fmt.Errorf("%s: failed to acquire upgrade->writer break-glass lock (pre-open): %w", command, err)
	}
	defer func() {
		if err := guard.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release direct DB locks: %w", err))
		}
	}()

	if err := deps.validateDirectPath(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target changed after acquiring locks; refusing to open SQLite: %w", command, err)
	}
	if err := deps.validateDirectLedger(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target is not a migratable clawctl ledger (locks acquired and will be released): %w", command, err)
	}
	if err := deps.checkMaintenance(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB break-glass rejected: %w", command, err)
	}
	if err := deps.verifyHubStopped(ctx, dbPath); err != nil {
		return fmt.Errorf("%s: direct DB break-glass rejected: %w", command, err)
	}
	// systemd proof may take several seconds. Recheck the pathname immediately
	// before SQLite sees it rather than trusting the pre-query identity.
	if err := deps.validateDirectPath(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB target changed after stopped proof; refusing to open SQLite: %w", command, err)
	}
	if err := deps.validateDirectLedger(dbPath); err != nil {
		return fmt.Errorf("%s: direct DB ledger changed after stopped proof; refusing writable open: %w", command, err)
	}
	st, err := deps.openDirectDB(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open direct DB %s: %w", dbPath, err)
	}
	if err := deps.validateDirectPath(dbPath); err != nil {
		return errors.Join(fmt.Errorf("%s: direct DB path changed after writable open: %w", command, err), st.Close())
	}
	if err := deps.validateDirectLedger(dbPath); err != nil {
		return errors.Join(fmt.Errorf("%s: direct DB ledger identity rejected after migration: %w", command, err), st.Close())
	}
	defer func() {
		if err := st.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close direct DB: %w", err))
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

const replayedRejectionNote = "; idempotency replay: this is the original verdict, not re-evaluated against current state"

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
		return fmt.Errorf("set channel failed: %w", err)
	}
	key, err := operatorRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("set channel failed: %w", err)
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
		return fmt.Errorf("set channel failed (idempotency-key=%q expected-revision=%d%s): %w",
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
		replay = "; idempotency replay"
	}
	_, err = fmt.Fprintf(out, "direct DB operator service: %s (%s): %s → %s; revision=%d; idempotency-key=%s%s\n",
		m.DisplayName, m.MachineID, before, after, result.Revision, key, replay)
	return err
}

func runMachineChannelHTTP(ctx context.Context, client *operatorclient.Client, inputs machineChannelInputs, out io.Writer) error {
	if strings.TrimSpace(inputs.Machine) == "" {
		return errors.New("set channel failed (HTTP operator API): --machine must be machine_id")
	}
	current, err := client.GetMachineChannel(ctx, inputs.Machine)
	if err != nil {
		return fmt.Errorf("set channel failed (HTTP operator API; --machine must be machine_id): %w", err)
	}
	key, err := operatorRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("set channel failed (HTTP operator API): %w", err)
	}
	expected := current.Revision
	if inputs.ExpectedRevision != nil {
		expected = *inputs.ExpectedRevision
	}
	result, err := client.PutMachineChannel(ctx, inputs.Machine, key, operatorclient.MachineChannelRequest{
		Channel: inputs.Set, ExpectedRevision: expected, ConfirmDisplayName: inputs.ConfirmName,
	})
	if err != nil {
		return fmt.Errorf("set channel failed (HTTP operator API; idempotency-key=%q expected-revision=%d%s): %w",
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
		replay = "; idempotency replay"
	}
	_, err = fmt.Fprintf(out, "HTTP operator API: %s (%s): %s → %s; revision=%d; ETag=%s; idempotency-key=%s%s\n",
		result.DisplayName, result.MachineID, before, after, result.Revision, result.Meta.ETag, key, replay)
	return err
}
