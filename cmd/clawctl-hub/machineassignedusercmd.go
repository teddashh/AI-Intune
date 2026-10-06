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
		fmt.Fprintln(errOut, "用法：clawctl-hub machine assigned-user --machine 機器識別碼 --user 使用者識別碼或none --confirm-name 機器名稱 [--hub-url URL | --db PATH]")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "機器識別碼；指定資料庫時也可使用名稱")
	user := fs.String("user", "", "Tailnet 使用者識別碼；none 取消指派")
	confirm := fs.String("confirm-name", "", "輸入機器名稱以確認指派")
	key := fs.String("idempotency-key", "", "重試時使用原請求金鑰")
	revision := fs.Int64("expected-revision", 0, "重試時使用原指派版本；與請求金鑰一起提供")
	hubURL := fs.String("hub-url", "", "Hub 網址")
	db := fs.String("db", "", "已停止 Hub 的資料庫路徑")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if fs.NArg() != 0 {
		return errors.New("不接受額外參數")
	}
	if strings.TrimSpace(*machine) == "" || strings.TrimSpace(*user) == "" || strings.TrimSpace(*confirm) == "" {
		return errors.New("請提供 --machine、--user 與 --confirm-name")
	}
	if seen["hub-url"] && seen["db"] {
		return errors.New("--hub-url 與 --db 不可同時提供")
	}
	if seen["idempotency-key"] != seen["expected-revision"] || (seen["idempotency-key"] && strings.TrimSpace(*key) == "") {
		return errors.New("請一起提供非空的 --idempotency-key 與 --expected-revision")
	}
	if (seen["db"] && strings.TrimSpace(*db) == "") || (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") {
		return errors.New("網址與資料庫路徑不可為空")
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
		return fmt.Errorf("指派失敗（請求金鑰=%q，預期版本=%d）：%w", key, expected, assignedUserCLIError(err))
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
		return fmt.Errorf("指派失敗（請求金鑰=%q，預期版本=%d）：%w", key, expected, assignedUserCLIError(err))
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
	return "未指派"
}

func writeAssignedUserCLI(out io.Writer, name, id, login string, revision int64, key string, replayed bool) error {
	replay := ""
	if replayed {
		replay = "；已重放"
	}
	_, err := fmt.Fprintf(out, "%s：指派使用者 %s；版本=%d；請求金鑰=%s%s\n", terminalSafe(name), assignedUserCLILabel(id, login), revision, terminalSafe(key), replay)
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
