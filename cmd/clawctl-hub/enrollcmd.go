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
		fmt.Fprintln(errOut, "Usage: clawctl-hub enroll-token [--ttl 24h] [--reason REASON] [--preview] [--hub-url URL | --db PATH] <display-name>")
		fmt.Fprintln(errOut, "      clawctl-hub enroll-token revoke [--reason REASON] [--preview] [--hub-url URL | --db PATH] <machine-id>")
		fmt.Fprintln(errOut, "  If display name happens to be revoke, separate explicitly with `clawctl-hub enroll-token [flags] -- revoke`.")
		fmt.Fprintln(errOut, "  Normal mode uses HTTP operator API: fetch expiry/impact preview first, then create single-use token.")
		fmt.Fprintln(errOut, "  --db is explicit stopped-service direct DB break-glass; requires existing canonical ledger, upgrade+writer locks, and stopped Hub unit.")
		fmt.Fprintln(errOut, "  After ambiguous response, only original name, TTL, reason, --idempotency-key, and --preview-digest may be reused; replay will not redisplay secret.")
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "existing SQLite file location for stopped-service direct DB break-glass")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered if omitted)")
	ttl := fs.Duration("ttl", defaultEnrollTokenTTL, "token validity duration (1m to 24h, whole seconds)")
	reason := fs.String("reason", "", "enrollment reason (recorded in audit, optional)")
	previewOnly := fs.Bool("preview", false, "preview expiry and denominator impact without generating token")
	idempotencyKey := fs.String("idempotency-key", "", "original request key; must be reused with --preview-digest")
	previewDigest := fs.String("preview-digest", "", "original preview digest; must be reused with --idempotency-key")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("enroll-token: exactly one display name must be specified")
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("enroll-token: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified explicitly")
	}
	if seen["hub-url"] && strings.TrimSpace(*hubURL) == "" {
		return errors.New("enroll-token: --hub-url cannot be empty")
	}
	if seen["db"] && strings.TrimSpace(*dbPath) == "" {
		return errors.New("enroll-token: --db cannot be empty")
	}
	if strings.TrimSpace(*idempotencyKey) == "" && seen["idempotency-key"] {
		return errors.New("enroll-token: --idempotency-key cannot be empty")
	}
	if strings.TrimSpace(*previewDigest) == "" && seen["preview-digest"] {
		return errors.New("enroll-token: --preview-digest cannot be empty")
	}
	if seen["preview-digest"] != seen["idempotency-key"] {
		return errors.New("enroll-token: --idempotency-key and --preview-digest must be provided together; omit both to create new request")
	}
	if *previewOnly && (seen["idempotency-key"] || seen["preview-digest"] || seen["reason"]) {
		return errors.New("enroll-token: --preview does not accept --idempotency-key, --preview-digest, or --reason; it does not create state")
	}
	if *ttl%time.Second != 0 {
		return errors.New("enroll-token: --ttl must be in whole seconds")
	}
	inputs := enrollTokenInputs{
		DisplayName: strings.TrimSpace(fs.Arg(0)), TTLSeconds: int64(*ttl / time.Second),
		Reason: *reason, IdempotencyKey: strings.TrimSpace(*idempotencyKey),
		PreviewDigest: strings.TrimSpace(*previewDigest), PreviewOnly: *previewOnly,
	}
	// Do not silently canonicalize a name whose request identity would differ
	// from what the operator typed. The domain service enforces the same rule.
	if inputs.DisplayName != fs.Arg(0) {
		return errors.New("enroll-token: display name cannot have leading or trailing whitespace")
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
			return errors.New("enroll-token: Hub discovery not initialized")
		}
		var err error
		selectedURL, err = deps.discoverHubURL()
		if err != nil {
			return fmt.Errorf("enroll-token: unable to discover Hub: %w", err)
		}
	}
	if deps.newOperatorClient == nil {
		return fmt.Errorf("enroll-token: %s client not initialized", mode)
	}
	client, err := deps.newOperatorClient(selectedURL)
	if err != nil {
		return fmt.Errorf("enroll-token: failed to configure %s: %w", mode, err)
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
			return fmt.Errorf("enroll-token preview (HTTP operator API) failed: %w", err)
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
		return fmt.Errorf("enroll-token: %w", err)
	}
	result, err := client.CreateEnrollToken(ctx, key, operatorclient.EnrollmentTokenCreateRequest{
		DisplayName: inputs.DisplayName, TTLSeconds: inputs.TTLSeconds,
		PreviewDigest: digest, Reason: inputs.Reason,
	})
	if err != nil {
		return fmt.Errorf("enroll-token create (HTTP operator API; idempotency-key=%q preview-digest=%q%s) failed: %w",
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
			return fmt.Errorf("enroll-token preview (direct DB operator service) failed: %w", err)
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
		return fmt.Errorf("enroll-token: %w", err)
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
		return fmt.Errorf("enroll-token create (direct DB operator service; idempotency-key=%q preview-digest=%q%s) failed: %w",
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
			"preview: %s cannot be created now: %d machines already on roster (retired excluded), limit %d machines; "+
				"to enroll more, retire unused machines first or raise the limit; preview-digest=%s\n",
			preview.DisplayName, preview.InDenominator, preview.LimitMaxMachines, preview.Digest)
		return err
	}
	_, err := fmt.Fprintf(w,
		"preview: %s will add one expected machine and enter denominator immediately; if created now, expires in %d seconds (%s); revocation does not remove roster row; preview-digest=%s\n",
		preview.DisplayName, preview.TTLSeconds, preview.ExpiresAt.UTC().Format(time.RFC3339),
		preview.Digest)
	return err
}

func finishEnrollToken(machineID, displayName, token string, secretAvailable, replayed,
	recoveryRequired bool, expiresAt time.Time, key, hubURL string, out, errOut io.Writer,
) error {
	if replayed || recoveryRequired || !secretAvailable || token == "" {
		return fmt.Errorf("enroll-token completed; token cannot be redisplayed; machine_id=%s; idempotency-key=%q; revoke: clawctl-hub enroll-token revoke %s",
			machineID, key, machineID)
	}
	// stdout has exactly one secret-bearing write. In particular, do not repeat
	// token in the example command or an error string.
	if _, err := fmt.Fprintln(out, token); err != nil {
		return fmt.Errorf("token delivery failed; machine_id=%s; idempotency-key=%q; revoke: clawctl-hub enroll-token revoke %s: %w",
			machineID, key, machineID, err)
	}
	base := hubURL
	if base == "" {
		base = "http://<Hub literal Tailscale IP>:<CLAWCTL_LISTEN port>"
	}
	_, err := fmt.Fprintf(errOut,
		"\n%s added to roster (machine_id %s).\n"+
			"token expires at %s and can only be used once.\n"+
			"./install-agent.sh --hub %s\n"+
			"./install-agent-macos.sh --hub %s\n"+
			".\\install-agent-windows.ps1 --hub %s\n"+
			"request key: %s\n",
		displayName, machineID, expiresAt.UTC().Format(time.RFC3339), base, base, base, key)
	return err
}
