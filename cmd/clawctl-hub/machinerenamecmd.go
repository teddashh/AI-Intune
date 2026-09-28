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
)

func runMachineRenameSubcommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("machine rename", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub machine rename --machine <id> --set <display_name> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  改 Hub 名冊名稱與名稱型 expectations；machine ID、agent 與機器 hostname 不變。")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "machine_id")
	set := fs.String("set", "", "新的 display_name")
	previewOnly := fs.Bool("preview", false, "只顯示影響，不寫入")
	reason := fs.String("reason", "", "重新命名理由")
	confirm := fs.String("confirm-name", "", "目前的 display_name")
	key := fs.String("idempotency-key", "", "重送原 request 使用的 key")
	previewDigest := fs.String("preview-digest", "", "重送原 request 使用的 preview digest")
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine rename: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*machine) == "" || strings.TrimSpace(*set) == "" {
		return errors.New("machine rename: --machine 與 --set 必填")
	}
	if *previewOnly && (*reason != "" || *confirm != "" || *key != "" || *previewDigest != "") {
		return errors.New("machine rename: --preview 不接受 apply 專用的 reason、confirm 或 replay 參數")
	}
	if !*previewOnly && (strings.TrimSpace(*reason) == "" || strings.TrimSpace(*confirm) == "") {
		return errors.New("machine rename: 套用時 --reason 與 --confirm-name 必填")
	}
	if (*key == "") != (*previewDigest == "") {
		return errors.New("machine rename: --idempotency-key 與 --preview-digest 必須一起提供")
	}
	explicit := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "hub-url" {
			explicit = true
		}
	})
	client, err := machinesHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return fmt.Errorf("machine rename: %w", err)
	}
	preview := operatorclient.MachineRenamePreviewResponse{}
	if *previewOnly || *previewDigest == "" {
		preview, err = client.PreviewMachineRename(ctx, *machine,
			operatorclient.MachineRenamePreviewRequest{DisplayName: *set})
		if err != nil {
			return fmt.Errorf("建立重新命名預覽失敗：%w", err)
		}
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		fmt.Fprintf(out, "%s → %s\n名稱型 expectations 改用新名稱；machine ID、agent 與機器上的 hostname 不變。\npreview digest %s\n",
			preview.CurrentDisplayName, preview.DisplayName, preview.PreviewDigest)
		if preview.PendingTokenLabelChanges {
			fmt.Fprintln(out, "尚未兌換的 enrollment ticket 標籤會一併更新；token 不變。")
		}
		return nil
	}
	requestKey, digest := *key, *previewDigest
	if digest == "" {
		digest = preview.PreviewDigest
		requestKey, err = operator.NewIdempotencyKey("cli-machine-rename")
		if err != nil {
			return fmt.Errorf("建立重新命名 request key 失敗：%w", err)
		}
	}
	result, err := client.PutMachineRename(ctx, *machine, requestKey, operatorclient.MachineRenameRequest{
		DisplayName: *set, ConfirmDisplayName: *confirm, PreviewDigest: digest, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("重新命名失敗：%w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	fmt.Fprintf(out, "%s → %s（machine %s）\n", result.PreviousDisplayName, result.DisplayName, result.MachineID)
	if result.PendingTokenLabelChanged {
		fmt.Fprintln(out, "尚未兌換的 enrollment ticket 標籤已一併更新；token 不變。")
	}
	if result.Replayed {
		fmt.Fprintln(out, "這是原 request 的成功回放；名冊沒有再次變更。")
	}
	return nil
}
