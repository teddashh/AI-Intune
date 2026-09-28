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

type enrollTokenRevokeInputs struct {
	MachineID      string
	Reason         string
	IdempotencyKey string
	PreviewDigest  string
	PreviewOnly    bool
}

func runEnrollTokenRevokeCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("enroll-token revoke", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub enroll-token revoke [--reason 原因] [--preview] [--hub-url URL | --db PATH] <machine-id>")
		fmt.Fprintln(errOut, "  影響：撤銷 pending enrollment ticket；machine 名冊、分母與 agent credential 不變。")
		fmt.Fprintln(errOut, "  transport：HTTP；--db 用於已停止的 Hub。")
		fmt.Fprintln(errOut, "  retry：成對重用 --idempotency-key 與 --preview-digest。")
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	reason := fs.String("reason", "", "撤銷原因（進 audit，選填）")
	previewOnly := fs.Bool("preview", false, "只顯示票證狀態與影響，不撤銷")
	idempotencyKey := fs.String("idempotency-key", "", "明示原 request key；必須與 --preview-digest 成對重用")
	previewDigest := fs.String("preview-digest", "", "明示原 preview digest；必須與 --idempotency-key 成對重用")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("enroll-token revoke: 必須指定一個 machine-id")
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("enroll-token revoke: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if seen["hub-url"] && strings.TrimSpace(*hubURL) == "" {
		return errors.New("enroll-token revoke: --hub-url 不可為空")
	}
	if seen["db"] && strings.TrimSpace(*dbPath) == "" {
		return errors.New("enroll-token revoke: --db 不可為空")
	}
	if seen["idempotency-key"] && strings.TrimSpace(*idempotencyKey) == "" {
		return errors.New("enroll-token revoke: --idempotency-key 不可為空")
	}
	if seen["preview-digest"] && strings.TrimSpace(*previewDigest) == "" {
		return errors.New("enroll-token revoke: --preview-digest 不可為空")
	}
	if seen["idempotency-key"] != seen["preview-digest"] {
		return errors.New("enroll-token revoke: --idempotency-key 與 --preview-digest 必須成對提供；省略兩者才會建立新的 request")
	}
	if *previewOnly && (seen["idempotency-key"] || seen["preview-digest"] || seen["reason"]) {
		return errors.New("enroll-token revoke: --preview 不接受 --idempotency-key、--preview-digest 或 --reason；它不建立 state")
	}
	machineID := strings.TrimSpace(fs.Arg(0))
	if machineID == "" || machineID != fs.Arg(0) {
		return errors.New("enroll-token revoke: machine-id 不可為空或帶有前後空白")
	}
	inputs := enrollTokenRevokeInputs{
		MachineID: machineID, Reason: *reason, PreviewOnly: *previewOnly,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey), PreviewDigest: strings.TrimSpace(*previewDigest),
	}

	if seen["db"] {
		return withDirectOperatorStore(ctx, "enroll-token revoke", *dbPath, deps, func(st *store.Store) error {
			return runEnrollTokenRevokeDirect(st, inputs, out, errOut)
		})
	}

	selectedURL := strings.TrimSpace(*hubURL)
	mode := "explicit HTTP operator API"
	if !seen["hub-url"] {
		mode = "discovered HTTP operator API"
		if deps.discoverHubURL == nil {
			return errors.New("enroll-token revoke: Hub discovery 未初始化")
		}
		var err error
		selectedURL, err = deps.discoverHubURL()
		if err != nil {
			return fmt.Errorf("enroll-token revoke: 無法發現 Hub：%w", err)
		}
	}
	if deps.newOperatorClient == nil {
		return fmt.Errorf("enroll-token revoke: %s client 未初始化", mode)
	}
	client, err := deps.newOperatorClient(selectedURL)
	if err != nil {
		return fmt.Errorf("enroll-token revoke: 設定 %s 失敗：%w", mode, err)
	}
	return runEnrollTokenRevokeHTTP(ctx, client, inputs, out, errOut)
}

func runEnrollTokenRevokeHTTP(ctx context.Context, client *operatorclient.Client,
	inputs enrollTokenRevokeInputs, out, errOut io.Writer,
) error {
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := client.PreviewEnrollmentTokenRevocation(ctx, inputs.MachineID)
		if err != nil {
			return fmt.Errorf("enroll-token revoke preview（HTTP operator API）失敗：%w", err)
		}
		if inputs.PreviewOnly {
			return writeEnrollTokenRevokePreview(out, preview.MachineID, preview.DisplayName,
				preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest)
		}
		if err := writeEnrollTokenRevokePreview(errOut, preview.MachineID, preview.DisplayName,
			preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := enrollTokenRevokeRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("enroll-token revoke：%w", err)
	}
	result, err := client.RevokeEnrollmentToken(ctx, inputs.MachineID, key,
		operatorclient.EnrollmentTokenRevocationRequest{PreviewDigest: digest, Reason: inputs.Reason})
	if err != nil {
		return fmt.Errorf("enroll-token revoke（HTTP operator API；idempotency-key=%q preview-digest=%q%s）失敗：%w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return writeEnrollTokenRevokeReceipt(out, result.MachineID, result.DisplayName,
		result.TokenWasExpired, result.RevokedAt, result.Replayed, key, digest)
}

func runEnrollTokenRevokeDirect(st *store.Store, inputs enrollTokenRevokeInputs, out, errOut io.Writer) error {
	service := operator.New(st)
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := service.PreviewEnrollTokenRevocation(operator.EnrollTokenRevocationPreviewRequest{
			MachineID: inputs.MachineID,
		})
		if err != nil {
			return fmt.Errorf("enroll-token revoke preview（direct DB operator service）失敗：%w", err)
		}
		if inputs.PreviewOnly {
			return writeEnrollTokenRevokePreview(out, preview.MachineID, preview.DisplayName,
				preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest)
		}
		if err := writeEnrollTokenRevokePreview(errOut, preview.MachineID, preview.DisplayName,
			preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := enrollTokenRevokeRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("enroll-token revoke：%w", err)
	}
	result, err := service.RevokeEnrollToken(operator.EnrollTokenRevocationRequest{
		MachineID: inputs.MachineID, PreviewDigest: digest, Reason: inputs.Reason,
		IdempotencyKey: key,
		Actor: operator.Actor{
			SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub enroll-token revoke", SourceKind: operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("enroll-token revoke（direct DB operator service；idempotency-key=%q preview-digest=%q%s）失敗：%w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return writeEnrollTokenRevokeReceipt(out, result.MachineID, result.DisplayName,
		result.TokenWasExpired, result.RevokedAt, result.Replayed, key, digest)
}

func enrollTokenRevokeRequestKey(supplied string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	return operator.NewIdempotencyKey("cli-enroll-token-revoke")
}

func writeEnrollTokenRevokePreview(w io.Writer, machineID, displayName string,
	expiresAt time.Time, expired bool, digest string,
) error {
	_, err := fmt.Fprintf(w,
		"preview: %s（machine_id %s）的待用 enrollment ticket 到期時間 %s（expired=%t）；撤銷後保留名冊、分母變化 0、已啟用 agent credential 不受影響；preview-digest=%s\n",
		displayName, machineID, expiresAt.UTC().Format(time.RFC3339), expired, digest)
	return err
}

func writeEnrollTokenRevokeReceipt(w io.Writer, machineID, displayName string,
	wasExpired bool, revokedAt time.Time, replayed bool, key, digest string,
) error {
	state := "revoked"
	if replayed {
		state = "replayed"
	}
	_, err := fmt.Fprintf(w,
		"%s: %s（machine_id %s）的待用 enrollment ticket 已撤銷（was_expired=%t，revoked_at=%s）；名冊保留、分母變化 0、已啟用 agent credential 不受影響；idempotency-key=%s preview-digest=%s\n",
		state, displayName, machineID, wasExpired, revokedAt.UTC().Format(time.RFC3339), key, digest)
	return err
}
