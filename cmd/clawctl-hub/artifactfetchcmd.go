package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"github.com/teddashh/AI-Intune/internal/store"
)

var artifactFetchCLIExactVersion = regexp.MustCompile(
	`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
)

type artifactFetchCLIBackend struct {
	source  string
	hubURL  string
	preview func(operator.ArtifactFetchPreviewRequest) (operator.ArtifactFetchPreviewResult, error)
	create  func(string, operator.ArtifactFetchApplyRequest) (operator.ArtifactFetchApplyResult, error)
	get     func(string) (store.ArtifactFetchOperation, error)
	run     func(store.ArtifactFetchOperation) (store.ArtifactFetchOperation, error)
}

type artifactFetchCLIInput struct {
	name           string
	version        string
	confirmVersion string
	reason         string
	idempotencyKey string
	previewDigest  string
	previewOnly    bool
	wait           bool
	pollInterval   time.Duration
	jsonOutput     bool
	recoveryFile   string
	recoveryLoaded bool
}

func runArtifactFetchCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runArtifactFetchCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runArtifactFetchCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if ctx == nil || out == nil || errOut == nil {
		return errors.New("artifact fetch: context 與輸出不可為 nil")
	}
	if len(argv) > 0 {
		switch argv[0] {
		case "list", "show":
			return runArtifactFetchReadCommandWithDeps(ctx, argv, out, errOut, deps)
		}
	}
	return runArtifactFetchMutationCommandWithDeps(ctx, argv, out, errOut, deps)
}

func runArtifactFetchMutationCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if forbidden := forbiddenArtifactFetchFlag(argv); forbidden != "" {
		return fmt.Errorf("artifact fetch: 不支援 --%s", forbidden)
	}
	target := ""
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		target, argv = argv[0], argv[1:]
	}
	needsValue := map[string]bool{
		"hub-url": true, "db": true, "confirm-version": true, "reason": true,
		"idempotency-key": true, "preview": false,
		"wait": false, "poll-interval": true, "json": false, "recovery-file": true,
	}
	if err := rejectDuplicateArtifactFetchFlags(argv, "artifact fetch", needsValue); err != nil {
		return err
	}

	fs := flag.NewFlagSet("artifact fetch", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub artifact fetch PACKAGE@VERSION --confirm-version VERSION --reason REASON --idempotency-key KEY [--wait] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub artifact fetch PACKAGE@VERSION --preview [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub artifact fetch --recovery-file ABSOLUTE_PATH [--wait] [--json]")
		fmt.Fprintln(errOut, "      clawctl-hub artifact fetch list|show ...")
		fmt.Fprintln(errOut, "  PACKAGE: openclaw、hermes-agent 或 node-runtime；VERSION 必須是完整版本。")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	confirmVersion := fs.String("confirm-version", "", "操作者明示確認的 exact version（enqueue 必填）")
	reason := fs.String("reason", "", "本次 intake 的理由（enqueue 必填，最多 500 bytes）")
	idempotencyKey := fs.String("idempotency-key", "", "明示 durable request key（enqueue 必填，重試須沿用）")
	recoveryFile := fs.String("recovery-file", "", "private canonical ambiguity-recovery receipt")
	previewOnly := fs.Bool("preview", false, "只顯示 exact impact，不 enqueue")
	wait := fs.Bool("wait", false, "HTTP enqueue 後輪詢到 succeeded 或 failed")
	pollInterval := fs.Duration("poll-interval", time.Second, "--wait 的輪詢間隔（>0 且不超過 1 minute）")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("artifact fetch: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if seen["hub-url"] {
		if err := validateArtifactFetchCLIValue("hub-url", *hubURL, 4096, false); err != nil {
			return err
		}
	}
	if seen["db"] {
		if err := validateArtifactFetchCLIValue("db", *dbPath, 4096, false); err != nil {
			return err
		}
	}
	if *pollInterval <= 0 || *pollInterval > time.Minute {
		return errors.New("artifact fetch: --poll-interval 必須大於 0 且不超過 1 minute")
	}
	if seen["poll-interval"] && !*wait {
		return errors.New("artifact fetch: --poll-interval 只能搭配 --wait")
	}
	if seen["recovery-file"] {
		document, exists, err := loadArtifactFetchRecoveryIfExists(*recoveryFile)
		if err != nil {
			return err
		}
		if exists {
			if target != "" || fs.NArg() != 0 {
				return errors.New("artifact fetch recovery replay 不接受 positional target")
			}
			for flagName := range seen {
				if flagName != "recovery-file" && flagName != "json" && flagName != "wait" && flagName != "poll-interval" {
					return errors.New("artifact fetch recovery replay 只接受 --recovery-file、--wait、--poll-interval 與 --json；request/transport 以 receipt 為準")
				}
			}
			input := artifactFetchCLIInput{
				name: document.Name, version: document.Version, confirmVersion: document.ConfirmVersion,
				reason: document.Reason, idempotencyKey: document.IdempotencyKey,
				previewDigest: document.PreviewDigest, wait: *wait, pollInterval: *pollInterval,
				jsonOutput: *jsonOutput, recoveryFile: *recoveryFile, recoveryLoaded: true,
			}
			client, resolvedURL, err := artifactFetchHTTPClient(document.HubURL, true, deps)
			if err != nil {
				return err
			}
			return executeArtifactFetchMutation(ctx, input,
				httpArtifactFetchCLIBackend(ctx, client, resolvedURL), out, errOut)
		}
	}
	if target == "" {
		if fs.NArg() != 1 {
			return errors.New("artifact fetch: 必須提供且只提供一個 PACKAGE@VERSION")
		}
		target = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("artifact fetch: 不接受多餘的 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	name, version, err := parseArtifactFetchCLITarget(target)
	if err != nil {
		return err
	}
	if *previewOnly {
		for _, flagName := range []string{"confirm-version", "reason", "idempotency-key", "wait", "poll-interval", "recovery-file"} {
			if seen[flagName] {
				return fmt.Errorf("artifact fetch --preview: 不接受 --%s", flagName)
			}
		}
	} else {
		if !seen["confirm-version"] || *confirmVersion != version {
			return errors.New("artifact fetch: --confirm-version 必填，且必須等於 target 的 exact version")
		}
		if !seen["reason"] {
			return errors.New("artifact fetch: --reason 必填")
		}
		if err := validateArtifactFetchCLIValue("reason", *reason, 500, false); err != nil {
			return err
		}
		if !seen["idempotency-key"] {
			return errors.New("artifact fetch: --idempotency-key 必填；ambiguous retry 必須沿用同一 key")
		}
		if err := validateArtifactFetchCLIValue("idempotency-key", *idempotencyKey, 200, false); err != nil {
			return err
		}
	}
	input := artifactFetchCLIInput{
		name: name, version: version, confirmVersion: *confirmVersion, reason: *reason,
		idempotencyKey: *idempotencyKey,
		previewOnly:    *previewOnly, wait: *wait, pollInterval: *pollInterval, jsonOutput: *jsonOutput,
		recoveryFile: *recoveryFile,
	}
	if seen["db"] {
		if seen["recovery-file"] {
			return errors.New("artifact fetch: direct DB operation 已 durable；--recovery-file 只適用 HTTP ambiguity")
		}
		return withDirectOperatorStore(ctx, "artifact fetch", *dbPath, deps, func(st *store.Store) error {
			service := operator.NewWithArtifacts(st, artifactsDirFor(*dbPath))
			backend := directArtifactFetchCLIBackend(ctx, service)
			return executeArtifactFetchMutation(ctx, input, backend, out, errOut)
		})
	}
	client, resolvedURL, err := artifactFetchHTTPClient(*hubURL, seen["hub-url"], deps)
	if err != nil {
		return err
	}
	return executeArtifactFetchMutation(ctx, input,
		httpArtifactFetchCLIBackend(ctx, client, resolvedURL), out, errOut)
}

func executeArtifactFetchMutation(ctx context.Context, input artifactFetchCLIInput,
	backend artifactFetchCLIBackend, out, errOut io.Writer,
) error {
	digest := input.previewDigest
	if digest == "" {
		preview, err := backend.preview(operator.ArtifactFetchPreviewRequest{
			Name: input.name, Version: input.version,
		})
		if err != nil {
			return fmt.Errorf("artifact fetch preview 失敗（%s）：%w", backend.source, err)
		}
		if input.previewOnly {
			return writeArtifactFetchPreview(out, preview, input.jsonOutput, backend.source)
		}
		if err := writeArtifactFetchPreview(errOut, preview, false, backend.source); err != nil {
			return err
		}
		if !preview.EnqueueAllowed || len(preview.Blockers) != 0 {
			return errors.New("artifact fetch: preview 不允許 enqueue")
		}
		digest = preview.PreviewDigest
	} else if !input.recoveryLoaded {
		return errors.New("artifact fetch: preview digest 只可來自已驗證的 private recovery receipt")
	} else if _, err := fmt.Fprintf(errOut,
		"%s；沿用 private recovery receipt 的 canonical preview digest %s，不重新取得 metadata。\n",
		backend.source, terminalSafe(digest)); err != nil {
		return err
	}
	input.previewDigest = digest
	var recovery *artifactFetchRecovery
	if backend.hubURL != "" {
		if input.recoveryFile == "" {
			path, err := defaultArtifactFetchRecoveryPath(input.idempotencyKey)
			if err != nil {
				return fmt.Errorf("artifact fetch 建立 private recovery path：%w", err)
			}
			input.recoveryFile = path
		}
		document := artifactFetchRecoveryFromInput(input, backend.hubURL)
		if err := ensureArtifactFetchRecovery(input.recoveryFile, document, input.recoveryLoaded); err != nil {
			return err
		}
		recovery = &document
		if err := writeArtifactFetchRecoveryInstruction(errOut, input.recoveryFile); err != nil {
			return err
		}
		if err := durablyVerifyArtifactFetchRecovery(input.recoveryFile, document); err != nil {
			return err
		}
	}
	apply, err := backend.create(input.idempotencyKey, operator.ArtifactFetchApplyRequest{
		Name: input.name, Version: input.version, PreviewDigest: digest,
		ConfirmName: input.name, ConfirmVersion: input.confirmVersion, Reason: input.reason,
	})
	if err != nil {
		enqueueErr := fmt.Errorf("artifact fetch enqueue 失敗（%s%s）：%w",
			backend.source, operatorRejectionReplayNote(err), err)
		if definitiveArtifactFetchRejection(err) {
			return errors.Join(enqueueErr, finishArtifactFetchRecovery(input.recoveryFile, recovery))
		}
		return enqueueErr
	}
	operation := apply.Operation
	if backend.run != nil {
		operation, err = backend.run(operation)
		if err != nil {
			return fmt.Errorf("artifact fetch direct worker 未到 terminal：%w", err)
		}
		if err := writeArtifactFetchOperation(out, operation, input.jsonOutput, backend.source, apply.Replayed); err != nil {
			return err
		}
		return artifactFetchTerminalError(operation)
	}
	if input.wait {
		operation, err = waitForArtifactFetchTerminal(ctx, operation, input.pollInterval, backend.get)
		if err != nil {
			return fmt.Errorf("輪詢 artifact fetch operation 失敗：%w", err)
		}
		if err := writeArtifactFetchOperation(out, operation, input.jsonOutput, backend.source, apply.Replayed); err != nil {
			return err
		}
		return errors.Join(artifactFetchTerminalError(operation),
			finishArtifactFetchRecovery(input.recoveryFile, recovery))
	}
	if input.jsonOutput {
		if err := writeArtifactReadJSON(out, apply); err != nil {
			return err
		}
	} else if err := writeArtifactFetchOperation(out, operation, false, backend.source, apply.Replayed); err != nil {
		return err
	}
	return finishArtifactFetchRecovery(input.recoveryFile, recovery)
}

func definitiveArtifactFetchRejection(err error) bool {
	var apiErr *operatorclient.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500
}

func finishArtifactFetchRecovery(path string, recovery *artifactFetchRecovery) error {
	if recovery == nil {
		return nil
	}
	return removeArtifactFetchRecovery(path, *recovery)
}

func runArtifactFetchReadCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	action := argv[0]
	args := argv[1:]
	operationID := ""
	if action == "show" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		operationID, args = args[0], args[1:]
	}
	needsValue := map[string]bool{
		"hub-url": true, "db": true, "state": true, "name": true,
		"version": true, "limit": true, "json": false,
	}
	if err := rejectDuplicateArtifactFetchFlags(args, "artifact fetch "+action, needsValue); err != nil {
		return err
	}
	fs := flag.NewFlagSet("artifact fetch "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub artifact fetch list [--state STATE] [--name PACKAGE] [--version VERSION] [--limit N] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub artifact fetch show <operation-id> [--json] [--hub-url URL | --db PATH]")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	state := fs.String("state", "", "只看 queued、running、succeeded 或 failed")
	name := fs.String("name", "", "只看 package（openclaw、hermes-agent 或 node-runtime）")
	version := fs.String("version", "", "只看 exact semver")
	limit := fs.Int("limit", store.DefaultArtifactFetchReadLimit, "最多幾筆（1..100）")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("artifact fetch status: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if seen["hub-url"] {
		if err := validateArtifactFetchCLIValue("hub-url", *hubURL, 4096, false); err != nil {
			return err
		}
	}
	if seen["db"] {
		if err := validateArtifactFetchCLIValue("db", *dbPath, 4096, false); err != nil {
			return err
		}
	}

	if action == "show" {
		for _, flagName := range []string{"state", "name", "version", "limit"} {
			if seen[flagName] {
				return fmt.Errorf("artifact fetch show: 不接受 --%s", flagName)
			}
		}
		if operationID == "" {
			if fs.NArg() != 1 {
				return errors.New("artifact fetch show: 必須提供且只提供一個 operation-id")
			}
			operationID = fs.Arg(0)
		} else if fs.NArg() != 0 {
			return errors.New("artifact fetch show: 只接受一個 operation-id")
		}
		if err := validateArtifactFetchOperationID(operationID); err != nil {
			return err
		}
		if seen["db"] {
			return withDirectOperatorStore(ctx, "artifact fetch show", *dbPath, deps, func(st *store.Store) error {
				service := operator.NewWithArtifacts(st, artifactsDirFor(*dbPath))
				operation, err := service.ArtifactFetchOperation(operationID)
				if err != nil {
					return fmt.Errorf("讀取 artifact fetch operation 失敗（direct DB operator service）：%w", err)
				}
				return writeArtifactFetchOperation(out, operation, *jsonOutput, "direct DB operator service", false)
			})
		}
		client, _, err := artifactFetchHTTPClient(*hubURL, seen["hub-url"], deps)
		if err != nil {
			return err
		}
		operation, err := client.ArtifactFetch(ctx, operationID)
		if err != nil {
			return fmt.Errorf("讀取 artifact fetch operation 失敗（HTTP operator API）：%w", err)
		}
		return writeArtifactFetchOperation(out, operation, *jsonOutput, "HTTP operator API", false)
	}

	if fs.NArg() != 0 {
		return fmt.Errorf("artifact fetch list: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	request := store.ArtifactFetchListRequest{State: store.ArtifactFetchState(*state), Name: *name, Version: *version, Limit: *limit}
	if request.State != "" && request.State != store.ArtifactFetchQueued &&
		request.State != store.ArtifactFetchRunning && request.State != store.ArtifactFetchSucceeded &&
		request.State != store.ArtifactFetchFailed {
		return errors.New("artifact fetch list: --state 只接受 queued、running、succeeded 或 failed")
	}
	if request.Name != "" && request.Name != "openclaw" && request.Name != "hermes-agent" && request.Name != "node-runtime" {
		return errors.New("artifact fetch list: --name 只接受 openclaw、hermes-agent 或 node-runtime")
	}
	if request.Version != "" && !artifactFetchCLIExactVersion.MatchString(request.Version) {
		return errors.New("artifact fetch list: --version 必須是 exact semver")
	}
	if request.Limit < 1 || request.Limit > store.MaxArtifactFetchReadLimit {
		return fmt.Errorf("artifact fetch list: --limit 必須介於 1 與 %d", store.MaxArtifactFetchReadLimit)
	}
	if seen["db"] {
		return withDirectOperatorStore(ctx, "artifact fetch list", *dbPath, deps, func(st *store.Store) error {
			result, err := operator.NewWithArtifacts(st, artifactsDirFor(*dbPath)).ArtifactFetchOperations(request)
			if err != nil {
				return fmt.Errorf("讀取 artifact fetch operations 失敗（direct DB operator service）：%w", err)
			}
			return writeArtifactFetchOperationList(out, result, *jsonOutput, "direct DB operator service")
		})
	}
	client, _, err := artifactFetchHTTPClient(*hubURL, seen["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.ArtifactFetches(ctx, request)
	if err != nil {
		return fmt.Errorf("讀取 artifact fetch operations 失敗（HTTP operator API）：%w", err)
	}
	return writeArtifactFetchOperationList(out, result, *jsonOutput, "HTTP operator API")
}

func httpArtifactFetchCLIBackend(ctx context.Context, client *operatorclient.Client,
	hubURL string,
) artifactFetchCLIBackend {
	return artifactFetchCLIBackend{
		source: "HTTP operator API", hubURL: hubURL,
		preview: func(request operator.ArtifactFetchPreviewRequest) (operator.ArtifactFetchPreviewResult, error) {
			return client.PreviewArtifactFetch(ctx, request)
		},
		create: func(key string, request operator.ArtifactFetchApplyRequest) (operator.ArtifactFetchApplyResult, error) {
			return client.CreateArtifactFetch(ctx, key, request)
		},
		get: func(operationID string) (store.ArtifactFetchOperation, error) {
			return client.ArtifactFetch(ctx, operationID)
		},
	}
}

func directArtifactFetchCLIBackend(ctx context.Context, service *operator.Service) artifactFetchCLIBackend {
	return artifactFetchCLIBackend{
		source: "direct DB operator service",
		preview: func(request operator.ArtifactFetchPreviewRequest) (operator.ArtifactFetchPreviewResult, error) {
			return service.PreviewArtifactFetch(ctx, request)
		},
		create: func(key string, request operator.ArtifactFetchApplyRequest) (operator.ArtifactFetchApplyResult, error) {
			request.IdempotencyKey = key
			request.Actor = operator.Actor{
				SourceAddr:     "local-cli",
				WhoUnavailable: "direct-db-cli",
				UserAgent:      "clawctl-hub artifact fetch", SourceKind: operator.SourceKindDirectDBCLI,
			}
			return service.ApplyArtifactFetch(ctx, request)
		},
		get: service.ArtifactFetchOperation,
		run: func(operation store.ArtifactFetchOperation) (store.ArtifactFetchOperation, error) {
			switch operation.State {
			case store.ArtifactFetchQueued:
				return service.RunArtifactFetchOperation(ctx, operation.OperationID, false)
			case store.ArtifactFetchRunning:
				// The stopped-service fence proves the former worker cannot still
				// hold authority. Rotate its run token and finish the durable work.
				return service.RunArtifactFetchOperation(ctx, operation.OperationID, true)
			case store.ArtifactFetchSucceeded, store.ArtifactFetchFailed:
				return operation, nil
			default:
				return operation, store.ErrArtifactFetchInvalidState
			}
		},
	}
}

func artifactFetchHTTPClient(explicitURL string, explicit bool,
	deps machineCommandDeps,
) (*operatorclient.Client, string, error) {
	candidate := explicitURL
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, "", errors.New("artifact fetch: operator HTTP client 未初始化")
		}
	} else {
		if deps.discoverHubURL == nil {
			return nil, "", errors.New("artifact fetch: Hub discovery 未初始化")
		}
		discovered, err := deps.discoverHubURL()
		if err != nil {
			return nil, "", fmt.Errorf("artifact fetch: 無法發現 Hub：%w", err)
		}
		candidate = discovered
		if deps.newOperatorClient == nil {
			return nil, "", errors.New("artifact fetch: operator HTTP client 未初始化")
		}
	}
	endpoint, err := operatorendpoint.ParseBaseURL(candidate)
	if err != nil {
		return nil, "", fmt.Errorf("artifact fetch: operator HTTP authority 不合法：%w", err)
	}
	resolvedURL := endpoint.BaseURL()
	client, err := deps.newOperatorClient(resolvedURL)
	if err != nil {
		if explicit {
			return nil, "", fmt.Errorf("artifact fetch: 建立 HTTP operator client 失敗：%w", err)
		}
		return nil, "", fmt.Errorf("artifact fetch: 建立 discovered HTTP operator client 失敗：%w", err)
	}
	return client, resolvedURL, nil
}

func waitForArtifactFetchTerminal(ctx context.Context, initial store.ArtifactFetchOperation,
	interval time.Duration, get func(string) (store.ArtifactFetchOperation, error),
) (store.ArtifactFetchOperation, error) {
	if artifactFetchTerminal(initial.State) {
		return initial, nil
	}
	if get == nil || interval <= 0 {
		return initial, errors.New("artifact fetch polling 未初始化")
	}
	for {
		operation, err := get(initial.OperationID)
		if err != nil {
			return initial, err
		}
		if operation.OperationID != initial.OperationID || operation.Name != initial.Name ||
			operation.Version != initial.Version || operation.IdentityDigest != initial.IdentityDigest ||
			operation.PreviewDigest != initial.PreviewDigest {
			return initial, errors.New("artifact fetch polling 回傳不同的 operation identity")
		}
		if artifactFetchTerminal(operation.State) {
			return operation, nil
		}
		initial = operation
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return initial, ctx.Err()
		case <-timer.C:
		}
	}
}

func artifactFetchTerminal(state store.ArtifactFetchState) bool {
	return state == store.ArtifactFetchSucceeded || state == store.ArtifactFetchFailed
}

func artifactFetchTerminalError(operation store.ArtifactFetchOperation) error {
	if operation.State != store.ArtifactFetchFailed {
		return nil
	}
	code := "ARTIFACT_FETCH_FAILED"
	if operation.ErrorCode != nil {
		code = *operation.ErrorCode
	}
	return fmt.Errorf("artifact fetch operation %s 結束為 failed（%s）",
		terminalSafe(operation.OperationID), terminalSafe(code))
}

func writeArtifactFetchPreview(out io.Writer, result operator.ArtifactFetchPreviewResult,
	jsonOutput bool, source string,
) error {
	if jsonOutput {
		return writeArtifactReadJSON(out, result)
	}
	existing := "none"
	if result.ExistingArtifactSHA256 != nil {
		existing = *result.ExistingArtifactSHA256
	}
	engines := "unspecified"
	if result.EnginesNode != nil {
		engines = *result.EnginesNode
	}
	impact := "download、verify、atomic publish"
	if result.AlreadyAvailableAndVerified {
		impact = "verified artifact 已存在；worker 可安全重用"
	}
	blockers := "none"
	if len(result.Blockers) != 0 {
		blockers = strings.Join(result.Blockers, ",")
	}
	_, err := fmt.Fprintf(out,
		"Artifact fetch（%s）\n  target: %s@%s\n  source kind: %s\n  source origin: %s\n  source integrity: %s\n  policy: %s\n  engines.node: %s\n  max bytes: %d\n  existing sha256: %s\n  action: %s\n  enqueue allowed: %t\n  blockers: %s\n  preview digest: %s\n",
		source, terminalSafe(result.Name), terminalSafe(result.Version),
		terminalSafe(result.SourceKind), terminalSafe(result.RegistryOrigin), terminalSafe(result.SHA512Integrity),
		terminalSafe(result.PolicyVersion),
		terminalSafe(engines), result.MaxBytes, terminalSafe(existing), impact, result.EnqueueAllowed,
		terminalSafe(blockers), terminalSafe(result.PreviewDigest))
	return err
}

func writeArtifactFetchOperation(out io.Writer, operation store.ArtifactFetchOperation,
	jsonOutput bool, source string, replayed bool,
) error {
	if jsonOutput {
		return writeArtifactReadJSON(out, operation)
	}
	replay := "fresh"
	if replayed {
		replay = "idempotency replay"
	}
	result := "pending"
	if operation.ResultSHA256 != nil && operation.ResultSizeBytes != nil {
		result = "sha256:" + *operation.ResultSHA256 + "/" + fmt.Sprint(*operation.ResultSizeBytes) + " bytes"
	} else if operation.ErrorCode != nil {
		result = "error=" + *operation.ErrorCode
		if operation.ErrorDetail != nil {
			result += " (" + *operation.ErrorDetail + ")"
		}
	}
	_, err := fmt.Fprintf(out,
		"%s；%s；operation %s；%s@%s；state=%s phase=%s progress=%d/%d attempt=%d；%s\n",
		source, replay, terminalSafe(operation.OperationID), terminalSafe(operation.Name),
		terminalSafe(operation.Version), terminalSafe(string(operation.State)), terminalSafe(string(operation.Phase)),
		operation.ProgressBytes, operation.MaxBytes, operation.Attempt, terminalSafe(result))
	return err
}

func writeArtifactFetchOperationList(out io.Writer, result store.ArtifactFetchListResult,
	jsonOutput bool, source string,
) error {
	if jsonOutput {
		return writeArtifactReadJSON(out, result)
	}
	if _, err := fmt.Fprintf(out, "%s；%s consistency；Hub 評估時間 %s；符合 %d 個。\n",
		source, terminalSafe(result.Consistency), result.EvaluatedAt.UTC().Format(time.RFC3339Nano), result.Total); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		_, err := fmt.Fprintln(out, "沒有符合 filter 的 artifact fetch operation。")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "OPERATION_ID\tTARGET\tSTATE\tPHASE\tPROGRESS\tATTEMPT\tUPDATED_AT\tRESULT"); err != nil {
		return err
	}
	for _, operation := range result.Items {
		resultText := "pending"
		if operation.ResultSHA256 != nil {
			resultText = "sha256:" + *operation.ResultSHA256
		} else if operation.ErrorCode != nil {
			resultText = *operation.ErrorCode
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s@%s\t%s\t%s\t%d/%d\t%d\t%s\t%s\n",
			terminalSafe(operation.OperationID), terminalSafe(operation.Name), terminalSafe(operation.Version),
			terminalSafe(string(operation.State)), terminalSafe(string(operation.Phase)), operation.ProgressBytes,
			operation.MaxBytes, operation.Attempt, operation.UpdatedAt.UTC().Format(time.RFC3339Nano),
			terminalSafe(resultText)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func parseArtifactFetchCLITarget(target string) (string, string, error) {
	name, version, ok := strings.Cut(target, "@")
	valid := false
	if ok {
		switch name {
		case "openclaw":
			valid = artifactFetchCLIExactVersion.MatchString(version)
		case "node-runtime":
			valid = artifact.ValidNodeRuntimeVersion(version)
		case "hermes-agent":
			valid = artifact.ValidHermesVersion(version)
		}
	}
	if !valid {
		return "", "", fmt.Errorf("artifact fetch: target 必須是 openclaw@<exact-semver>、hermes-agent@<major.minor.patch> 或 node-runtime@<major.minor.patch>，拿到 %s", terminalSafe(target))
	}
	return name, version, nil
}

func validateArtifactFetchCLIValue(name, value string, maxBytes int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || len(value) > maxBytes || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) {
		return fmt.Errorf("artifact fetch: --%s 不可為空、過長、非 UTF-8 或含首尾空白", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("artifact fetch: --%s 不可含控制或隱形格式字元", name)
		}
	}
	return nil
}

func validateArtifactFetchOperationID(value string) error {
	if err := validateArtifactFetchCLIValue("operation-id", value, 128, false); err != nil ||
		value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return errors.New("artifact fetch show: operation-id 不可為空、過長、含首尾空白、控制字元、dot segment 或斜線")
	}
	return nil
}

func forbiddenArtifactFetchFlag(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "registry", "max-bytes", "by":
			return name
		}
	}
	return ""
}

func rejectDuplicateArtifactFetchFlags(args []string, command string,
	needsValue map[string]bool,
) error {
	seen := make(map[string]bool, len(needsValue))
	consumeValue := false
	for _, arg := range args {
		if consumeValue {
			consumeValue = false
			continue
		}
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		name, _, hasEquals := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		needsNext, known := needsValue[name]
		if !known {
			continue
		}
		if seen[name] {
			return fmt.Errorf("%s: --%s 不可重複", command, name)
		}
		seen[name] = true
		consumeValue = needsNext && !hasEquals
	}
	return nil
}
