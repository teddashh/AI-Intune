package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func runMachineAssignedUserSubcommand(ctx context.Context, argv []string, out, errOut io.Writer, deps machineCommandDeps) error {
	fs := flag.NewFlagSet("machine assigned-user", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine assigned-user --machine MACHINE_ID --user USER_ID_OR_NONE --confirm-name MACHINE_NAME [--hub-url URL | --db PATH]")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "machine ID; name may also be used when specifying database")
	user := fs.String("user", "", "Tailnet user ID; none to unassign")
	confirm := fs.String("confirm-name", "", "machine name to confirm assignment")
	key := fs.String("idempotency-key", "", "original request key when retrying")
	revision := fs.Int64("expected-revision", 0, "original assignment revision when retrying; provide with request key")
	hubURL := fs.String("hub-url", "", "Hub URL")
	db := fs.String("db", "", "database path of stopped Hub")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if fs.NArg() != 0 {
		return errors.New("positional arguments not accepted")
	}
	if strings.TrimSpace(*machine) == "" || strings.TrimSpace(*user) == "" || strings.TrimSpace(*confirm) == "" {
		return errors.New("--machine, --user, and --confirm-name are required")
	}
	if seen["hub-url"] && seen["db"] {
		return errors.New("--hub-url and --db cannot be provided together")
	}
	if seen["idempotency-key"] != seen["expected-revision"] || (seen["idempotency-key"] && strings.TrimSpace(*key) == "") {
		return errors.New("non-empty --idempotency-key and --expected-revision must be provided together")
	}
	if (seen["db"] && strings.TrimSpace(*db) == "") || (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") {
		return errors.New("URL and database path cannot be empty")
	}
	inputs := machineAssignedUserInputs{Machine: *machine, UserID: *user, ConfirmName: *confirm, IdempotencyKey: *key}
	if seen["expected-revision"] {
		inputs.ExpectedRevision = revision
	}
	if seen["db"] {
		return withDirectOperatorStore(ctx, "machine assigned-user", *db, deps, func(st *store.Store) error { return runMachineAssignedUser(ctx, st, deps.tailnetSource, inputs, out) })
	}
	client, err := machinesHTTPClient(*hubURL, seen["hub-url"], deps)
	if err != nil {
		return err
	}
	return runMachineAssignedUserHTTP(ctx, client, inputs, out)
}

type machineAssignedUserInputs struct {
	Machine, UserID, ConfirmName, IdempotencyKey string
	ExpectedRevision                             *int64
}

func assignedUserCLIKey(supplied string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	return operator.NewIdempotencyKey("cli-machine-assigned-user")
}

func runMachineAssignedUser(ctx context.Context, st *store.Store, source operator.TailnetSource, inputs machineAssignedUserInputs, out io.Writer) error {
	m, err := resolveMachine(st, inputs.Machine, true)
	if err != nil {
		return err
	}
	key, err := assignedUserCLIKey(inputs.IdempotencyKey)
	if err != nil {
		return err
	}
	expected := m.AssignedUserRevision
	if inputs.ExpectedRevision != nil {
		expected = *inputs.ExpectedRevision
	}
	result, err := operator.NewWithTailnet(st, source).ChangeMachineAssignedUser(ctx, operator.MachineAssignedUserRequest{
		MachineID: m.MachineID, UserID: inputs.UserID, ExpectedRevision: &expected, ConfirmDisplayName: inputs.ConfirmName, IdempotencyKey: key,
		Actor: operator.Actor{SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli", UserAgent: "clawctl-hub machine assigned-user", SourceKind: operator.SourceKindDirectDBCLI},
	})
	if err != nil {
		return fmt.Errorf("assignment failed (request key=%q, expected revision=%d): %w", key, expected, assignedUserCLIError(err))
	}
	return writeAssignedUserCLI(out, result.DisplayName, result.UserID, result.UserLogin, result.Revision, key, result.Replayed)
}

func runMachineAssignedUserHTTP(ctx context.Context, client *operatorclient.Client, inputs machineAssignedUserInputs, out io.Writer) error {
	current, err := client.GetMachineAssignedUser(ctx, inputs.Machine)
	if err != nil {
		return err
	}
	key, err := assignedUserCLIKey(inputs.IdempotencyKey)
	if err != nil {
		return err
	}
	expected := current.Revision
	if inputs.ExpectedRevision != nil {
		expected = *inputs.ExpectedRevision
	}
	result, err := client.PutMachineAssignedUser(ctx, inputs.Machine, key, operatorclient.MachineAssignedUserRequest{UserID: inputs.UserID, ExpectedRevision: expected, ConfirmDisplayName: inputs.ConfirmName})
	if err != nil {
		return fmt.Errorf("assignment failed (request key=%q, expected revision=%d): %w", key, expected, assignedUserCLIError(err))
	}
	return writeAssignedUserCLI(out, result.DisplayName, result.UserID, result.UserLogin, result.Revision, key, result.Replayed)
}

func assignedUserCLILabel(id, login string) string {
	if login != "" {
		return terminalSafe(login)
	}
	if id != "" {
		return terminalSafe(id)
	}
	return "unassigned"
}

func writeAssignedUserCLI(out io.Writer, name, id, login string, revision int64, key string, replayed bool) error {
	replay := ""
	if replayed {
		replay = "; replayed"
	}
	_, err := fmt.Fprintf(out, "%s: assigned user %s; revision=%d; request key=%s%s\n", terminalSafe(name), assignedUserCLILabel(id, login), revision, terminalSafe(key), replay)
	return err
}

func assignedUserCLIError(err error) error {
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Code == store.OperatorCodeTailnetSourceUnavailable {
		copy := *rejection
		copy.Detail = "使用者名冊：來源不可用"
		return &copy
	}
	return err
}
