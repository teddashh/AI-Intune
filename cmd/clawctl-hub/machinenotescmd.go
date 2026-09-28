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

func runMachineNotesSubcommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("machine notes", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub machine notes --machine <id> --set <notes> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  更新 Hub 名冊中的人工備註；--set '' 清除備註，機器設定與 agent 不變。")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "machine_id")
	set := fs.String("set", "", "新的 notes；空字串表示清除")
	previewOnly := fs.Bool("preview", false, "只顯示影響，不寫入")
	reason := fs.String("reason", "", "更新備註理由")
	confirm := fs.String("confirm-name", "", "目前的 display_name")
	key := fs.String("idempotency-key", "", "重送原 request 使用的 key")
	previewDigest := fs.String("preview-digest", "", "重送原 request 使用的 preview digest")
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine notes: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	seen := map[string]bool{}
	fs.Visit(func(item *flag.Flag) { seen[item.Name] = true })
	if strings.TrimSpace(*machine) == "" || !seen["set"] {
		return errors.New("machine notes: --machine 與 --set 必填；--set '' 表示清除")
	}
	if *previewOnly && (*reason != "" || *confirm != "" || *key != "" || *previewDigest != "") {
		return errors.New("machine notes: --preview 不接受 apply 專用的 reason、confirm 或 replay 參數")
	}
	if !*previewOnly && (strings.TrimSpace(*reason) == "" || strings.TrimSpace(*confirm) == "") {
		return errors.New("machine notes: 套用時 --reason 與 --confirm-name 必填")
	}
	if (*key == "") != (*previewDigest == "") {
		return errors.New("machine notes: --idempotency-key 與 --preview-digest 必須一起提供")
	}
	explicit := seen["hub-url"]
	client, err := machinesHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return fmt.Errorf("machine notes: %w", err)
	}
	preview := operatorclient.MachineNotesPreviewResponse{}
	if *previewOnly || *previewDigest == "" {
		preview, err = client.PreviewMachineNotes(ctx, *machine,
			operatorclient.MachineNotesPreviewRequest{Notes: *set})
		if err != nil {
			return fmt.Errorf("建立名冊備註預覽失敗：%w", err)
		}
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		current, desired := terminalSafe(preview.CurrentNotes), terminalSafe(preview.Notes)
		if current == "" {
			current = "（無備註）"
		}
		if desired == "" {
			desired = "（清除備註）"
		}
		fmt.Fprintf(out, "%s：%s → %s\n只更新 Hub 名冊；機器設定與 agent 不變。\npreview digest %s\n",
			terminalSafe(preview.DisplayName), current, desired, preview.PreviewDigest)
		return nil
	}
	requestKey, digest := *key, *previewDigest
	if digest == "" {
		digest = preview.PreviewDigest
		requestKey, err = operator.NewIdempotencyKey("cli-machine-notes")
		if err != nil {
			return fmt.Errorf("建立名冊備註 request key 失敗：%w", err)
		}
	}
	result, err := client.PutMachineNotes(ctx, *machine, requestKey, operatorclient.MachineNotesRequest{
		Notes: *set, ConfirmDisplayName: *confirm, PreviewDigest: digest, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("更新名冊備註失敗：%w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.NotesPresent {
		fmt.Fprintf(out, "%s（machine %s）的名冊備註已更新；機器設定與 agent 不變。\n",
			terminalSafe(result.DisplayName), terminalSafe(result.MachineID))
	} else {
		fmt.Fprintf(out, "%s（machine %s）的名冊備註已清除；機器設定與 agent 不變。\n",
			terminalSafe(result.DisplayName), terminalSafe(result.MachineID))
	}
	if result.Replayed {
		fmt.Fprintln(out, "這是原 request 的成功回放；名冊沒有再次變更。")
	}
	return nil
}
