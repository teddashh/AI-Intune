package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

const defaultEnrollTokenTTL = 24 * time.Hour

func runEnrollTokenCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runEnrollTokenCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

// runEnrollTokenCommandWithDeps keeps the normal path HTTP-only. The shared
// dependency bundle is intentional: enrollment and machine-channel direct DB
// modes must pass through the exact same stopped-service proof and lock order.
func runEnrollTokenCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if len(argv) > 0 && argv[0] == "revoke" {
		return runEnrollTokenRevokeCommandWithDeps(ctx, argv[1:], out, errOut, deps)
	}
	fs := flag.NewFlagSet("enroll-token", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub enroll-token [--ttl 24h] [--reason 原因] [--preview] [--hub-url URL | --db PATH] <顯示名稱>")
		fmt.Fprintln(errOut, "      clawctl-hub enroll-token revoke [--reason 原因] [--preview] [--hub-url URL | --db PATH] <machine-id>")
		fmt.Fprintln(errOut, "  顯示名稱若剛好是 revoke，請用 `clawctl-hub enroll-token [flags] -- revoke` 明確分隔。")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API：先取得 expiry/impact preview，再建立只顯示一次的 token。")
		fmt.Fprintln(errOut, "  --db 是明示的 stopped-service direct DB break-glass；要求既有 canonical ledger、upgrade+writer locks，且受控 Hub unit 完全停止。")
		fmt.Fprintln(errOut, "  ambiguous response 後只可重用原本的 name、TTL、reason、--idempotency-key 與 --preview-digest；replay 不會重顯 secret。")
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	ttl := fs.Duration("ttl", defaultEnrollTokenTTL, "這張票多久過期（1m 到 24h，整秒）")
	reason := fs.String("reason", "", "為什麼要納管（進 audit，選填）")
	previewOnly := fs.Bool("preview", false, "只顯示到期時間與分母影響，不開票")
	idempotencyKey := fs.String("idempotency-key", "", "明示原 request key；必須與 --preview-digest 成對重用")
	previewDigest := fs.String("preview-digest", "", "明示原 preview digest；必須與 --idempotency-key 成對重用")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("enroll-token: 必須指定一個顯示名稱")
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("enroll-token: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if seen["hub-url"] && strings.TrimSpace(*hubURL) == "" {
		return errors.New("enroll-token: --hub-url 不可為空")
	}
	if seen["db"] && strings.TrimSpace(*dbPath) == "" {
		return errors.New("enroll-token: --db 不可為空")
	}
	if strings.TrimSpace(*idempotencyKey) == "" && seen["idempotency-key"] {
		return errors.New("enroll-token: --idempotency-key 不可為空")
	}
	if strings.TrimSpace(*previewDigest) == "" && seen["preview-digest"] {
		return errors.New("enroll-token: --preview-digest 不可為空")
	}
	if seen["preview-digest"] != seen["idempotency-key"] {
		return errors.New("enroll-token: --idempotency-key 與 --preview-digest 必須成對提供；省略兩者才會建立新的 request")
	}
	if *previewOnly && (seen["idempotency-key"] || seen["preview-digest"] || seen["reason"]) {
		return errors.New("enroll-token: --preview 不接受 --idempotency-key、--preview-digest 或 --reason；它不建立 state")
	}
	if *ttl%time.Second != 0 {
		return errors.New("enroll-token: --ttl 必須是整秒")
	}
	inputs := enrollTokenInputs{
		DisplayName: strings.TrimSpace(fs.Arg(0)), TTLSeconds: int64(*ttl / time.Second),
		Reason: *reason, IdempotencyKey: strings.TrimSpace(*idempotencyKey),
		PreviewDigest: strings.TrimSpace(*previewDigest), PreviewOnly: *previewOnly,
	}
	// Do not silently canonicalize a name whose request identity would differ
	// from what the operator typed. The domain service enforces the same rule.
	if inputs.DisplayName != fs.Arg(0) {
		return errors.New("enroll-token: 顯示名稱開頭或結尾不可有空白")
	}

	if seen["db"] {
		return withDirectOperatorStore(ctx, "enroll-token", *dbPath, deps, func(st *store.Store) error {
			return runEnrollTokenDirect(st, inputs, out, errOut)
		})
	}

	selectedURL := strings.TrimSpace(*hubURL)
	mode := "explicit HTTP operator API"
	if !seen["hub-url"] {
		mode = "discovered HTTP operator API"
		if deps.discoverHubURL == nil {
			return errors.New("enroll-token: Hub discovery 未初始化")
		}
		var err error
		selectedURL, err = deps.discoverHubURL()
		if err != nil {
			return fmt.Errorf("enroll-token: 無法發現 Hub：%w", err)
		}
	}
	if deps.newOperatorClient == nil {
		return fmt.Errorf("enroll-token: %s client 未初始化", mode)
	}
	client, err := deps.newOperatorClient(selectedURL)
	if err != nil {
		return fmt.Errorf("enroll-token: 設定 %s 失敗：%w", mode, err)
	}
	return runEnrollTokenHTTP(ctx, client, selectedURL, inputs, out, errOut)
}

type enrollTokenInputs struct {
	DisplayName    string
	TTLSeconds     int64
	Reason         string
	IdempotencyKey string
	PreviewDigest  string
	PreviewOnly    bool
}

func runEnrollTokenHTTP(ctx context.Context, client *operatorclient.Client, hubURL string,
	inputs enrollTokenInputs, out, errOut io.Writer,
) error {
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := client.PreviewEnrollToken(ctx, operatorclient.EnrollmentTokenPreviewRequest{
			DisplayName: inputs.DisplayName, TTLSeconds: inputs.TTLSeconds,
		})
		if err != nil {
			return fmt.Errorf("enroll-token preview（HTTP operator API）失敗：%w", err)
		}
		previewCopy := enrollTokenPreviewCopy{
			DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
			ExpiresAt: preview.ExpiresAtIfCreatedNow, Digest: preview.PreviewDigest,
			InDenominator: preview.InDenominator, LimitMaxMachines: preview.LimitMaxMachines,
			AtLimit: preview.AtLimit,
		}
		if inputs.PreviewOnly {
			return writeEnrollTokenPreview(out, previewCopy)
		}
		if err := writeEnrollTokenPreview(errOut, previewCopy); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := enrollTokenRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("enroll-token：%w", err)
	}
	result, err := client.CreateEnrollToken(ctx, key, operatorclient.EnrollmentTokenCreateRequest{
		DisplayName: inputs.DisplayName, TTLSeconds: inputs.TTLSeconds,
		PreviewDigest: digest, Reason: inputs.Reason,
	})
	if err != nil {
		return fmt.Errorf("enroll-token create（HTTP operator API；idempotency-key=%q preview-digest=%q%s）失敗：%w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return finishEnrollToken(result.MachineID, result.DisplayName, result.EnrollmentToken,
		result.SecretAvailable, result.Replayed, result.RecoveryRequired,
		result.ExpiresAt, key, strings.TrimRight(hubURL, "/"), out, errOut)
}

func runEnrollTokenDirect(st *store.Store, inputs enrollTokenInputs, out, errOut io.Writer) error {
	service := operator.New(st)
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := service.PreviewEnrollToken(operator.EnrollTokenPreviewRequest{
			DisplayName: inputs.DisplayName, TTLSeconds: inputs.TTLSeconds,
		})
		if err != nil {
			return fmt.Errorf("enroll-token preview（direct DB operator service）失敗：%w", err)
		}
		previewCopy := enrollTokenPreviewCopy{
			DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
			ExpiresAt: preview.ExpiresAtIfCreatedNow, Digest: preview.PreviewDigest,
			InDenominator: preview.InDenominator, LimitMaxMachines: preview.LimitMaxMachines,
			AtLimit: preview.AtLimit,
		}
		if inputs.PreviewOnly {
			return writeEnrollTokenPreview(out, previewCopy)
		}
		if err := writeEnrollTokenPreview(errOut, previewCopy); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := enrollTokenRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("enroll-token：%w", err)
	}
	result, err := service.CreateEnrollToken(operator.EnrollTokenCreateRequest{
		DisplayName: inputs.DisplayName, TTLSeconds: inputs.TTLSeconds,
		PreviewDigest: digest, Reason: inputs.Reason, IdempotencyKey: key,
		Actor: operator.Actor{
			SourceAddr:     "local-cli",
			WhoUnavailable: "direct-db-cli",
			UserAgent:      "clawctl-hub enroll-token", SourceKind: operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("enroll-token create（direct DB operator service；idempotency-key=%q preview-digest=%q%s）失敗：%w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return finishEnrollToken(result.MachineID, result.DisplayName, result.EnrollmentToken,
		result.SecretAvailable, result.Replayed, result.RecoveryRequired,
		result.ExpiresAt, key, "", out, errOut)
}

func enrollTokenRequestKey(supplied string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	return operator.NewIdempotencyKey("cli-enroll-token")
}

// enrollTokenPreviewCopy 是這一行預覽要講的全部事實，兩個來源（HTTP 與 direct DB）
// 各自填同一組欄位，才不會只有其中一條路講得出上限。
type enrollTokenPreviewCopy struct {
	DisplayName      string
	TTLSeconds       int64
	ExpiresAt        time.Time
	Digest           string
	InDenominator    int
	LimitMaxMachines int
	AtLimit          bool
}

// writeEnrollTokenPreview 說出這一張票現在建立會發生什麼。
//
// ⚠ 到了註冊上限就不可以再講「會新增一台並立即進分母」。那句話描述的是一個不會發生的
// 結果——下一個動作就會被擋下來——而一份預告錯結果的預覽，會讓操作的人把那次拒絕當成
// 偶發失敗去重試。網頁在按鈕旁邊講的是同一件事，terminal 不能少講。
func writeEnrollTokenPreview(w io.Writer, preview enrollTokenPreviewCopy) error {
	if preview.AtLimit {
		_, err := fmt.Fprintf(w,
			"preview: %s 現在建立不了：名冊上已有 %d 台（已退役的不算），上限 %d 台；"+
				"要再納管就先退役不用的機器，或把上限調高；preview-digest=%s\n",
			preview.DisplayName, preview.InDenominator, preview.LimitMaxMachines, preview.Digest)
		return err
	}
	_, err := fmt.Fprintf(w,
		"preview: %s 會新增一台 expected machine 並立即進分母；若現在建立，%d 秒後（%s）過期；撤票不移除名冊列；preview-digest=%s\n",
		preview.DisplayName, preview.TTLSeconds, preview.ExpiresAt.UTC().Format(time.RFC3339),
		preview.Digest)
	return err
}

func finishEnrollToken(machineID, displayName, token string, secretAvailable, replayed,
	recoveryRequired bool, expiresAt time.Time, key, hubURL string, out, errOut io.Writer,
) error {
	if replayed || recoveryRequired || !secretAvailable || token == "" {
		return fmt.Errorf("enroll-token 已完成；token 不可重顯；machine_id=%s；idempotency-key=%q；撤銷：clawctl-hub enroll-token revoke %s",
			machineID, key, machineID)
	}
	// stdout has exactly one secret-bearing write. In particular, do not repeat
	// token in the example command or an error string.
	if _, err := fmt.Fprintln(out, token); err != nil {
		return fmt.Errorf("token delivery 失敗；machine_id=%s；idempotency-key=%q；撤銷：clawctl-hub enroll-token revoke %s：%w",
			machineID, key, machineID, err)
	}
	base := hubURL
	if base == "" {
		base = "http://<Hub 的 literal Tailscale IP>:<CLAWCTL_LISTEN 的埠>"
	}
	_, err := fmt.Fprintf(errOut,
		"\n%s 已加入名冊（machine_id %s）。\n"+
			"token 在 %s 過期，只能使用一次。\n"+
			"./install-agent.sh --hub %s\n"+
			"request key: %s\n",
		displayName, machineID, expiresAt.UTC().Format(time.RFC3339), base, key)
	return err
}
