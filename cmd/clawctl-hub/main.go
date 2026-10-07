// clawctl-hub 是機隊的那一台。它收心跳、收觀測、下判決、每天早上講一次話。
//
// ⚠ 它不在資料平面上。它從來不主動連進任何一台機器，也不是 BAT 或 SSH 的
// 代理。它掛掉的時候，所有機器照常工作，你只是不知道它們的狀況 ——
// 這正是我們要的失效模式。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/blobstore"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/objectref"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"github.com/teddashh/AI-Intune/internal/report"
	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
	"github.com/teddashh/AI-Intune/internal/web"
	"tailscale.com/net/tsaddr"
)

var version = "dev"

type hub struct {
	store      *store.Store
	tailnet    *tailnet.Cache
	agentLinks *agentlink.Registry

	// operatorService is the one artifact-aware control-plane service owned by
	// this Hub process. The startup recovery worker and runtime queue worker must
	// share it so they use one fixed fetch policy and artifacts directory.
	operatorService *operator.Service

	// hubHost 是這個 process 的 --hub-host：Hub 自己那台機器的名字。
	// Dashboard 用它偵測「Hub 裝在它自己管的機器上」，verifier registration
	// 用它把 hub_prober 的 failure domain 解析成同一個 machine_id namespace。
	hubHost string

	// artifactsDir 只放 Hub 已下載、驗過 sha512 並自行算過 sha256 的 tarball。
	// Machine download routes 只從這裡讀；只有受 admin 保護的 operator
	// artifact-intake worker 會依固定 policy 連 production registry。
	artifactsDir string

	// blobs 是選擇性的 R2/S3 耐久副本。nil 代表沿用本機 artifacts 目錄，
	// 不建立 object_blobs 列。有設定時，SQLite 只留 digest 與 object key。
	blobs blobstore.Backend

	// drillStamp：上一次還原演練蓋的章（restoredrill.go）。空字串 = 不看。
	drillStamp string

	// startedAt：這個 process 起來的時間。吐成 clawctl_hub_started_timestamp_seconds，
	// 讓 Prometheus 那一側也看得到「Hub 剛重啟過」（changes() 就問得出來）。
	startedAt time.Time

	// expects 是人宣告的「每個 unit 該留下什麼」。
	// ⚠ 它可能是「沒設定」也可能是「設定了但讀壞了」，兩者不一樣，
	// 所以整個 Set 都留著，不是只留 rules。見 internal/expect。
	expects *expect.Set
	// notifyCmd is the legacy --notify-cmd / CLAWCTL_NOTIFY_CMD. When set it
	// wins over built-in channels: the child still inherits CLAWCTL_NOTIFY_ENV
	// so ops/notify-telegram.sh keeps working.
	notifyCmd string
	// notifyEnv is the path from --notify-env / CLAWCTL_NOTIFY_ENV. Secrets
	// are read from that file only, never from flags or the process environment.
	notifyEnv string
	// notifyBuiltin is set when prepareNotify selected the built-in channels.
	notifyBuiltin *builtinNotifier
	// notifyKind is "command", "telegram", "webhook", or "telegram+webhook".
	notifyKind string
	// notifyEnvChannelsIgnored is true when a notify command is used even
	// though the env file also defines built-in channels.
	notifyEnvChannelsIgnored bool
	reportAt                 string // 本地時間 HH:MM

	// reportStamp 是外部死人之鐘要讀的那個檔（ops/deadman.sh）。
	//
	// ⚠⚠ 它**只在早報真的送達之後**才會被更新，而且只有 daily 那一種。
	//
	// 很自然會想用 cron 每分鐘 `date -u +%s > stamp`，那樣簡單得多。
	// 但那個時間戳的意思會變成「cron 還活著」，而不是「早報有送到」——
	// 於是 Telegram token 被撤銷、reportLoop 卡死、buildReport panic 這三種
	// 情況下，死人之鐘全部維持綠燈。它會變成裝飾品。
	//
	// 自證不算數這條規則對 Hub 自己也適用：Hub 唯一有資格蓋的章，
	// 是「有一則早報離開了這台機器並且對方收下了」。
	reportStamp string

	// reportPingURL 是外部死人之鐘的 push 端點（healthchecks.io 或等價物）。
	//
	// ⚠ 為什麼有了 reportStamp 還要這個：deadman.sh 住在 sampleagent2，
	// 它擋得住 samplehub1 掛掉，但擋不住 sampleagent2 自己掛掉，也擋不住整個機隊
	// 一起掛掉（停電、網路、雲端商出事）。這一條完全在機隊之外。
	// 兩條不是重複，是不同的失效面。
	//
	// ⚠ URL 本身就是憑證：誰知道它誰就能讓死人之鐘閉嘴。不准印進 log。
	reportPingURL     string
	reportPingFailURL string

	// publicURL 是早報連結與 Web 產生的 agent enroll 指令共用的可信前綴，
	// 沒有尾斜線。serve path 必定有值；standalone report preview 才可能為空。
	//
	// ⚠ 只能從已驗證的 literal Tailscale `--listen` 推導，不准相信 request Host。
	// CLAWCTL_PUBLIC_URL 現在只可重申同一個 URL，不能覆寫成第二個 authority。
	// 理由不是保密（repo 已經是私人的），是 §5.12：一個寫死的位址在寫下它
	// 的那台機器上永遠是對的，在別台上永遠是錯的 —— 而 Connect 那個
	// `https://{{IP}}:8080` 就是這樣在四台實機上全錯了好幾天。
	// report preview 推不出位址時**不放連結**，並且把理由印進 stderr。
	publicURL string

	// retention 是每一類資料留多久。零值是不合法的（Validate 會擋），
	// 所以 serve 一定要填 —— 一個零值的政策會被解讀成「全部刪掉」。
	retention store.RetentionPolicy
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	if len(os.Args) > 1 {
		// Upgrade preflight: prove the configured source-to-destination Tailscale
		// grant before the upgrade script stops the live Hub or snapshots its DB.
		// Like the rollback handshakes below, this path must not open the store.
		if os.Args[1] == "--operator-auth-check" {
			if err := runOperatorAuthCheck(os.Args[1:], os.Stdout); err != nil {
				log.Fatalf("operator auth preflight 失敗：%v", err)
			}
			return
		}
		// 這是 binary capability handshake，不是一般會開 DB/migrate 的 serve。
		// 舊 binary 不認得這個 flag，會在 Open 前由 flag package 拒絕；upgrade
		// script 因此不會把「程式跑得起來」誤當成「懂新版 rollback 契約」。
		if os.Args[1] == "--rollback-compatible" {
			if err := runRollbackCompatibility(os.Args[1:], os.Stdout); err != nil {
				log.Fatalf("拒絕 rollback：%v", err)
			}
			return
		}
		if os.Args[1] == "--rollback-snapshot" {
			if err := runRollbackSnapshot(os.Args[1:], os.Stdout); err != nil {
				log.Fatalf("拒絕 rollback snapshot：%v", err)
			}
			return
		}
		if os.Args[1] == "--upgrade-maintenance-begin" {
			if err := runUpgradeMaintenanceBegin(os.Args[1:], os.Stdout, disableCurrentExecutableForMaintenance); err != nil {
				log.Fatalf("拒絕進入 upgrade maintenance：%v", err)
			}
			return
		}
		if os.Args[1] == "--upgrade-stopped-check" {
			if err := runUpgradeStoppedCheck(context.Background(), os.Args[1:], os.Stdout); err != nil {
				log.Fatalf("拒絕 upgrade stopped proof：%v", err)
			}
			return
		}
		if os.Args[1] == "--upgrade-protocol-check" {
			if err := runUpgradeProtocolCheck(os.Args[1:], os.Stdout); err != nil {
				log.Fatalf("拒絕 upgrade protocol proof：%v", err)
			}
			return
		}
		if os.Args[1] == "--upgrade-ledger-path-check" {
			if err := runUpgradeLedgerPathCheck(os.Args[1:], os.Stdout); err != nil {
				log.Fatalf("拒絕 upgrade ledger path proof：%v", err)
			}
			return
		}
		command, err := classifyTopLevel(os.Args[1:])
		if err != nil {
			log.Fatal(err)
		}
		if err := rejectTopLevelCLIWhileUpgradeMaintenance(command, os.Args[1:]); err != nil {
			log.Fatal(err)
		}
		switch command {
		case "enroll-token":
			cmdEnrollToken(os.Args[2:])
			return
		case "machines":
			cmdMachines(os.Args[2:])
			return
		case "audit":
			cmdAudit(os.Args[2:])
			return
		case "retire":
			cmdRetire(os.Args[2:])
			return
		case "report":
			switch {
			case len(os.Args) > 2 && os.Args[2] == "changes":
				cmdReportChanges(os.Args[3:])
			case len(os.Args) > 2 && os.Args[2] == "list":
				cmdReportList(os.Args[3:])
			case len(os.Args) > 2 && os.Args[2] == "enrollment":
				cmdReportEnrollment(os.Args[3:])
			case len(os.Args) > 2 && os.Args[2] == "software":
				cmdReportSoftware(os.Args[3:])
			case len(os.Args) > 2 && os.Args[2] == "install":
				cmdReportInstall(os.Args[3:])
			case len(os.Args) > 2 && os.Args[2] == "profile":
				cmdReportProfile(os.Args[3:])
			default:
				cmdReport(os.Args[2:])
			}
			return
		case "data":
			cmdDataDisclosure(os.Args[2:])
			return
		case "tickets":
			cmdTickets(os.Args[2:])
			return
		case "enrollment-limit":
			cmdEnrollmentLimit(os.Args[2:])
			return
		case "tailnet":
			cmdTailnet(os.Args[2:])
			return
		case "prune":
			cmdPrune(os.Args[2:])
			return
		case "restore-drill":
			cmdRestoreDrill(os.Args[2:])
			return
		case "job":
			cmdJob(os.Args[2:])
			return
		case "machine":
			cmdMachine(os.Args[2:])
			return
		case "deployment":
			cmdDeployment(os.Args[2:])
			return
		case "artifact":
			cmdArtifact(os.Args[2:])
			return
		case "catalog":
			cmdCatalog(os.Args[2:])
			return
		case "verifier":
			cmdVerifier(os.Args[2:])
			return
		case "settings":
			cmdSettings(os.Args[2:])
			return
		case "compliance":
			cmdCompliance(os.Args[2:])
			return
		case "version":
			fmt.Println(version)
			return
		case "notify-check":
			cmdNotifyCheck(os.Args[2:])
			return
		}
	}
	serve(os.Args[1:])
}

func runOperatorAuthCheck(argv []string, out io.Writer) error {
	return runOperatorAuthCheckWithFactory(argv, out, func(config operatorauth.Config) (operatorRequestAuthorizer, error) {
		return operatorauth.New(config)
	})
}

// runOperatorAuthCheckWithFactory is the testable core of the pre-activation
// grant probe. It deliberately constructs an in-memory request: Authorize only
// asks tailscaled's LocalAPI about the configured source and destination, and
// no TCP listener or SQLite file is needed for this decision.
func runOperatorAuthCheckWithFactory(argv []string, out io.Writer,
	newAuthorizer func(operatorauth.Config) (operatorRequestAuthorizer, error),
) error {
	fs := flag.NewFlagSet("operator-auth-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("operator-auth-check", false, "在換版前驗證 operator app capabilities")
	listen := fs.String("listen", os.Getenv("CLAWCTL_LISTEN"), "即將啟動的 literal Tailscale listener")
	prefix := fs.String("operator-capability-prefix", os.Getenv("CLAWCTL_OPERATOR_CAPABILITY_PREFIX"),
		"Tailscale grants app capability 前綴")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*check {
		return errors.New("缺少 --operator-auth-check")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("多餘參數：%s", strings.Join(fs.Args(), " "))
	}
	destination, err := operatorDestinationFromListen(*listen)
	if err != nil {
		return fmt.Errorf("operator console listener %q：%w", *listen, err)
	}
	authority, err := operatorAuthorityFromListen(*listen)
	if err != nil {
		return fmt.Errorf("operator console listener authority %q：%w", *listen, err)
	}
	if _, why := publicBase(*listen); why != "" {
		return fmt.Errorf("operator console 位址設定不合法：%s", why)
	}
	if !tsaddr.IsTailscaleIP(destination) {
		return fmt.Errorf("listener destination %q 必須是明確的 Tailscale IP", destination)
	}
	// The rollout owner is fixed to the Hub node itself. An overridable source
	// would let an operator prove a source/destination pair that the post-start
	// local verification never exercises.
	source := destination

	authorizer, err := newAuthorizer(operatorauth.Config{
		Destination: destination, CapabilityPrefix: *prefix,
	})
	if err != nil {
		return fmt.Errorf("operator auth 設定不合法：%w", err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+authority+"/", nil)
	if err != nil {
		return fmt.Errorf("建立 operator auth probe：%w", err)
	}
	request.RemoteAddr = net.JoinHostPort(source.String(), "1")
	before := routingOf(request)
	authed, decision := authorizer.Authorize(request, operatorauth.Admin)
	if !decision.Allowed || authed == nil {
		if decision.Allowed || authed != nil || !validAuthorizationDenial(decision) {
			return fmt.Errorf("operator auth adapter 回傳不一致的拒絕判決：allowed=%t request_nil=%t status=%d code=%q",
				decision.Allowed, authed == nil, decision.HTTPStatus, decision.Code)
		}
		return fmt.Errorf("source %s → destination %s 的 admin capability：%s（%s）",
			source, destination, decision.Code, decision.Detail)
	}
	principal, breach := validateAuthorizedResult(before, authed, decision, operatorauth.Admin)
	if breach != "" {
		return fmt.Errorf("operator auth adapter 的 success 判決不完整：%s", breach)
	}
	// Authorize reads and validates the complete CapMap in one WhoIs response.
	// Check all three keys from that coherent snapshot; three separate calls
	// could accidentally combine three policy-propagation moments into a set of
	// capabilities that never existed together.
	for _, permission := range []operatorauth.Permission{operatorauth.View, operatorauth.Operate, operatorauth.Admin} {
		if !principal.Has(permission) {
			return fmt.Errorf("source %s 缺少獨立的 %s capability；不做程式內權限繼承", source, permission)
		}
	}
	fmt.Fprintf(out, "operator-auth-ready:v1 source=%s destination=%s authority=%s capabilities=view,operate,admin self-only=true\n",
		source, destination, authority)
	return nil
}

// classifyTopLevel separates positional subcommands from the flag-only serve
// invocation before anything can open or migrate the database.  flag.FlagSet
// stops parsing at an unknown positional instead of rejecting it; without this
// guard, a typo such as `rollback-snapshot --help` silently became `serve`.
// The empty command means the Hub server (no args, or flags beginning with -).
func classifyTopLevel(argv []string) (string, error) {
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		return "", nil
	}
	switch argv[0] {
	case "enroll-token", "machines", "audit", "retire", "report", "data", "tickets", "tailnet",
		"enrollment-limit", "prune", "restore-drill", "job", "machine", "deployment",
		"artifact", "catalog", "verifier", "settings", "compliance", "version", "notify-check":
		return argv[0], nil
	default:
		return "", fmt.Errorf("不認得命令 %q；啟動 Hub 時只接受 --listen、--db 等 flags，或子指令（例如 notify-check）", argv[0])
	}
}

// rejectUnexpectedServePositionals runs after flag parsing but before Store.Open.
// It catches a positional typo hidden behind otherwise valid serve flags.
func rejectUnexpectedServePositionals(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	return fmt.Errorf("serve 不接受 positional 參數：%q", strings.Join(argv, " "))
}

func runRollbackCompatibility(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("rollback-compatible", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("rollback-compatible", false, "唯讀確認 ledger 已靜止，可安全交給支援此契約的舊 Hub")
	dbPath := fs.String("db", defaultDB(), "SQLite 檔位置")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("缺少 --rollback-compatible")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("多餘參數：%s", strings.Join(fs.Args(), " "))
	}
	if err := store.CheckRollbackCompatible(*dbPath); err != nil {
		return err
	}
	fmt.Fprintln(out, "rollback-compatible：ledger 已靜止（0 active deployments（running/paused），0 非終態 jobs，0 active artifact fetches（queued/running））")
	return nil
}

// ---------------------------------------------------------------- serve

func serve(argv []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:8770", "監聽位址（literal Tailscale IP:port；未指定時讀 $CLAWCTL_LISTEN）。旗標預設 127.0.0.1:8770 是 fail-closed placeholder，Hub 會拒絕。沒有正式環境預設埠；8787 只是文件裡的慣例範例，grant dst、CLAWCTL_PUBLIC_URL、agent --hub、tunnel origin 必須與這個位址同一個埠。")
	dbPath := fs.String("db", defaultDB(), "SQLite 檔位置")
	hubHost := fs.String("hub-host", hostname(), "這台的名字；用來偵測 Hub 是不是裝在它自己管的機器上")
	notify := fs.String("notify-cmd", os.Getenv("CLAWCTL_NOTIFY_CMD"), "legacy command that receives the report on stdin; when set, built-in channels are not used")
	notifyEnv := fs.String("notify-env", os.Getenv("CLAWCTL_NOTIFY_ENV"), "path to the notify env file (Telegram/webhook secrets; never taken from flags or the process environment)")
	reportAt := fs.String("report-at", "08:00", "每天送早報的本地時間 HH:MM")
	stamp := fs.String("report-stamp", os.Getenv("CLAWCTL_REPORT_STAMP"),
		"早報送達後把 unix 時間寫進這個檔；外部死人之鐘讀它（見 ops/deadman.sh）")
	def := store.DefaultRetention()
	keepObs := fs.Duration("keep-observations", def.Observations, "觀測留多久")
	keepCheckins := fs.Duration("keep-checkins", def.Checkins, "心跳留多久")
	keepOccupancy := fs.Duration("keep-occupancy", def.Occupancy, "票的占用帳留多久")
	operatorCapabilityPrefix := fs.String("operator-capability-prefix", os.Getenv("CLAWCTL_OPERATOR_CAPABILITY_PREFIX"),
		"Tailscale grants app capability 前綴（<owned-domain>/cap/<application>）")
	_ = fs.Parse(argv)
	listenSet := false
	fs.Visit(func(f *flag.Flag) { listenSet = listenSet || f.Name == "listen" })
	if !listenSet {
		*addr = serveListenDefault()
	}
	if err := rejectUnexpectedServePositionals(fs.Args()); err != nil {
		log.Fatal(err)
	}
	canonicalDBPath, err := canonicalServeDBPath(*dbPath)
	if err != nil {
		log.Fatalf("資料庫路徑無法 canonicalize：%v", err)
	}
	*dbPath = canonicalDBPath
	destination, err := operatorDestinationFromListen(*addr)
	if err != nil {
		log.Fatalf("operator auth 設定不合法：拒絕 --listen %q：%v", *addr, err)
	}
	operatorAuthority, err := operatorAuthorityFromListen(*addr)
	if err != nil {
		log.Fatalf("operator HTTP authority 拒絕 --listen %q：%v", *addr, err)
	}
	operatorAuthorizer, err := operatorauth.New(operatorauth.Config{
		Destination: destination, CapabilityPrefix: *operatorCapabilityPrefix,
	})
	if err != nil {
		log.Fatalf("operator auth 設定不合法：%v；請設定 CLAWCTL_OPERATOR_CAPABILITY_PREFIX", err)
	}
	operatorBase, why := publicBase(*addr)
	if why != "" {
		log.Fatalf("operator console 位址設定不合法：%s", why)
	}
	objectBlobs, objectBlobSummary, err := blobstore.FromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("object storage 設定不合法：%v", err)
	}
	notifySetup, err := prepareNotify(*notify, *notifyEnv)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if notifySetup.modeWarn != "" {
		log.Print(notifySetup.modeWarn)
	}
	// Own the exact configured address before opening/migrating SQLite. A
	// syntactically valid 100.x address can belong to another tailnet peer; if
	// we wait until the end of startup to bind it, a bad config can touch the
	// ledger and only then discover EADDRNOTAVAIL.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("監聽 %s 失敗（pre-open）：%v", *addr, err)
	}
	defer ln.Close()

	// The lifecycle lock intentionally is not taken here: upgrade-hub starts
	// this candidate while it still owns <db>.upgrade.lock.  The separate writer
	// lock prevents a second Hub (even on another port) or a stopped-service
	// direct operator from opening the same ledger at the same time.
	writerGuard, err := ledgerlock.AcquireWriter(*dbPath)
	if err != nil {
		log.Fatalf("取得資料庫 writer lock 失敗（pre-open）%s：%v", ledgerlock.WriterPath(*dbPath), err)
	}
	defer func() {
		if err := writerGuard.Close(); err != nil {
			log.Printf("釋放資料庫 writer lock 失敗 %s：%v", writerGuard.Path(), err)
		}
	}()

	st, err := openServeStore(*dbPath)
	if err != nil {
		log.Fatalf("開不了資料庫 %s：%v", *dbPath, err)
	}
	defer st.Close()
	if err := sweepOpenAgentSessionsOnStartup(st, log.Printf); err != nil {
		log.Fatalf("無法確認 Hub 重新啟動後的終端狀態：Hub 不會開始服務；請修復資料庫後重新啟動：%v", err)
	}

	exps := loadExpectations(st)
	// deployment CLI 是另一個 process，不會繼承 systemd EnvironmentFile。
	// 在開始接 observation 前把 service 真正載入的 policy 發布進同一個 DB，
	// CLI 的 promote gate 才不會在 expects=nil 的另一個世界下判斷。
	if err := st.PublishExpectationsPolicy(time.Now().UTC()); err != nil {
		log.Fatalf("發布 workload expectations policy 失敗：%v", err)
	}

	artifactsDir := artifactsDirFor(*dbPath)
	tailnetCache := tailnet.NewCache()
	operatorService := operator.NewControlPlane(st, artifactsDir, tailnetCache)
	operatorService.ConfigureRestoreDrill(restoredrill.Runner{
		BackupsDir: filepath.Join(filepath.Dir(*dbPath), "backups"),
		StampPath:  drillStampPath(*dbPath),
		Live:       st,
	})
	if objectBlobs != nil {
		operatorService.SetArtifactBlobPublisher(objectref.Publisher{Backend: objectBlobs, Store: st})
		log.Printf("object storage: %s", objectBlobSummary)
	}
	h := &hub{
		store: st, tailnet: tailnetCache, agentLinks: agentlink.New(), artifactsDir: artifactsDir, blobs: objectBlobs, operatorService: operatorService, publicURL: operatorBase,
		hubHost:   *hubHost,
		startedAt: time.Now(), drillStamp: drillStampPath(*dbPath),
		notifyCmd: notifySetup.cmd, notifyEnv: notifySetup.envPath,
		notifyBuiltin: notifySetup.builtin, notifyKind: notifySetup.kind,
		notifyEnvChannelsIgnored: notifySetup.cmdHidesBuiltin,
		reportAt:                 *reportAt, reportStamp: *stamp,
		expects:           exps,
		reportPingURL:     os.Getenv("CLAWCTL_REPORT_PING_URL"),
		reportPingFailURL: os.Getenv("CLAWCTL_REPORT_PING_FAIL_URL"),
		retention: store.RetentionPolicy{
			Observations: *keepObs, Checkins: *keepCheckins, Occupancy: *keepOccupancy,
		},
	}
	// ⚠ 開機就擋掉一個會切進讀取窗口的保留期，而不是等到半夜第一次清理才發現。
	// 一個在凌晨三點才炸掉的設定錯誤，等於它上線的那天到炸掉的那天之間，
	// 沒有任何東西被清 —— 而 log 裡不會有人看的那一行。
	if err := h.retention.Validate(); err != nil {
		log.Fatalf("保留期設定不合法：%v", err)
	}
	log.Printf("保留期：觀測 %v、心跳 %v、票的占用帳 %v（每 %v 清一次，看 retention_log）",
		h.retention.Observations, h.retention.Checkins, h.retention.Occupancy, pruneInterval)
	// ⚠ 開機時把「失敗時會不會告警」講清楚。不講的話，一個帶 query string
	// 的 URL 會讓失敗告警永遠靜音，而那件事只有在真的出事那天才會被發現。
	if h.reportPingURL != "" {
		switch {
		case h.reportPingFailURL != "":
			log.Print("外部死人之鐘：成功與失敗各有自己的 ping URL")
		case failPingURL(h.reportPingURL) != "":
			log.Print("外部死人之鐘：失敗時會 ping <url>/fail（healthchecks 慣例）")
		default:
			log.Print("⚠ 外部死人之鐘：ping URL 帶了 query string，推不出失敗端點。" +
				"早報送不出去時不會立即告警，只能等對方超時。" +
				"要立即告警請設 CLAWCTL_REPORT_PING_FAIL_URL。")
		}
	}
	h.logNotifyConfigured()

	ui, err := web.New(st, *hubHost)
	if err != nil {
		log.Fatalf("樣板載入失敗：%v", err)
	}
	ui.SetDrillStampReader(func() (time.Time, bool) { return readDrillStamp(h.drillStamp) })
	ui.SetTerminalLinks(h.agentLinks)
	installOperatorTerminalSessionCloser(h)
	ui.SetArtifactsDir(h.artifactsDir)
	ui.SetTailnetCache(tailnetCache)
	ui.SetOperatorService(operatorService)
	ui.SetHubBase(h.publicURL)
	if err := ui.SetRetentionPolicy(h.retention); err != nil {
		log.Fatalf("網頁保留期設定不合法：%v", err)
	}
	if err := ui.SetAgentBundles(filepath.Join(filepath.Dir(*dbPath), "agent-bootstrap", version), version); err != nil {
		log.Fatalf("Agent bootstrap bundles 載入失敗：%v", err)
	}

	handler, err := newHubHTTPHandler(h, ui, operatorAuthorizer, operatorAuthority)
	if err != nil {
		log.Fatalf("operator route policy 不完整：%v", err)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: maintenanceMiddleware(upgradeMaintenanceMarker(*dbPath), handler),
		// ⚠ 這幾個 timeout 不是裝飾。一個卡住的連線會佔著 goroutine，
		// 而 Hub 是單點 —— 它不能因為一台機器的網路很爛就變慢。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if recovered, err := recoverArtifactFetchWorker(ctx, h.operatorService); err != nil {
		if ctx.Err() != nil {
			log.Print("artifact fetch startup recovery canceled before Hub became ready")
			return
		}
		// The underlying error can contain the private registry path or local
		// artifact directory. It is deliberately not interpolated into the log.
		log.Fatal("artifact fetch startup recovery failed; private detail suppressed")
	} else if recovered > 0 {
		log.Printf("artifact fetch startup recovery fenced %d running operation(s); READY-time worker will resume them", recovered)
	}
	if recovered, err := recoverRestoreDrillWorker(ctx, h.operatorService); err != nil {
		if ctx.Err() != nil {
			log.Print("restore drill startup recovery canceled before Hub became ready")
			return
		}
		log.Fatal("restore drill startup recovery failed; private detail suppressed")
	} else if recovered > 0 {
		log.Printf("restore drill startup recovery fenced %d running operation(s); READY-time worker will resume them", recovered)
	}

	// ⚠ port 已在開 DB 之前佔起來；現在才跟 systemd 說 READY。
	//
	// 用 ListenAndServe 然後立刻 notifyReady()，會在「port 已經被佔用」時
	// 先回報就緒、再 Fatal —— systemd 看到的是「起來了然後馬上掛」，
	// 而不是「根本沒起來」。這兩件事的排查方向完全不同。
	// ⚠ 起來的第一件事是記「我起來了、上一次活著是多久前」。
	// 這一筆是早報「三台同時失聯」唯一能拿來對的東西。
	h.journalStart(time.Now())

	go h.reconcileLoop(ctx)
	go h.agentSessionRevocationLoop(ctx)
	go h.jobReaperLoop(ctx)
	go h.reportLoop(ctx)
	go h.pruneLoop(ctx)
	go watchdogLoop(ctx, h)

	go func() {
		log.Printf("clawctl-hub %s 啟動於 http://%s（資料庫 %s）", version, *addr, *dbPath)
		notifyReady()
		// Artifact recovery above is intentionally local-only. Start registry and
		// tarball work strictly after READY so an unavailable origin cannot keep the
		// control plane offline during startup.
		go artifactFetchWorkerLoop(ctx, h.operatorService)
		go restoreDrillWorkerLoop(ctx, h.operatorService)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服務停止：%v", err)
		}
	}()

	<-ctx.Done()
	log.Print("收到停止訊號，正在收尾")
	h.journal(store.HubStopping, "收到停止訊號（kill -9 不會有這一筆，那是預期的）", time.Now())
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.agentLinks.CloseAll(agentlink.ReasonAgentDisconnected)
	_ = srv.Shutdown(shutCtx)
}

// serveListenDefault returns the --listen flag default: CLAWCTL_LISTEN when
// set, otherwise the historical fail-closed loopback placeholder (still
// rejected by ParseListen before the DB opens). Docker/distroless images have
// no shell to expand ${CLAWCTL_LISTEN} in CMD, so the binary must read env.
func serveListenDefault() string {
	if v := os.Getenv("CLAWCTL_LISTEN"); v != "" {
		return v
	}
	return "127.0.0.1:8770"
}

func operatorDestinationFromListen(listen string) (netip.Addr, error) {
	endpoint, err := operatorendpoint.ParseListen(listen)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("必須是 canonical literal Tailscale listener IP:nonzero-port（且為 node address）：%w", err)
	}
	return endpoint.Destination(), nil
}

func operatorAuthorityFromListen(listen string) (string, error) {
	endpoint, err := operatorendpoint.ParseListen(listen)
	if err != nil {
		return "", fmt.Errorf("必須是 canonical literal Tailscale listener IP:nonzero-port（且為 node address）：%w", err)
	}
	return endpoint.Authority(), nil
}

// ---------------------------------------------------------------- systemd

// watchdogLoop 餵 systemd 的看門狗。
//
// ⚠ 它刻意**先打一次資料庫**再回報還活著。只送 WATCHDOG=1 而不碰資料庫，
// 那就變成「這個 process 的 goroutine 還在排程」—— 一個 SQLite 檔案鎖死、
// 什麼查詢都跑不動的 Hub 會很有自信地一路餵下去，而 systemd 永遠不會重啟它。
// 自證不算數這條規則對 Hub 自己也適用。
//
// 但它也不是健康檢查。真正的 Hub 健康判定在外部死人之鐘那裡（ops/deadman.sh）：
// 早報有沒有準時送到。這裡只負責讓「卡死」變成「重啟」。
func watchdogLoop(ctx context.Context, h *hub) {
	usec := os.Getenv("WATCHDOG_USEC")
	if usec == "" {
		return // 不是 systemd 起的，沒有看門狗要餵
	}
	var micros int64
	if _, err := fmt.Sscanf(usec, "%d", &micros); err != nil || micros <= 0 {
		return
	}
	// 間隔取 WatchdogSec 的三分之一：漏掉一次不會立刻被殺。
	every := time.Duration(micros/3) * time.Microsecond
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.store.PingReader(ctx); err != nil {
				// ⚠ 不餵。讓 systemd 殺掉重來 —— 一個查不動資料庫的 Hub
				// 還活著，比它死掉更糟：它會安靜地什麼都不回報。
				log.Printf("看門狗：資料庫沒有回應，停止餵食：%v", err)
				continue
			}
			notifyWatchdog()
		}
	}
}

// sdNotify 跟 systemd 講話。
//
// ⚠ 它會把失敗**印出來**。原本這裡是靜默 return，而那讓一個
// 「Type=notify 但 READY 永遠送不到」的部署看起來像是程式卡住了 ——
// systemd 只說 "start operation timed out"，log 裡什麼線索都沒有。
// 一個吞掉錯誤的通知函式，跟這個專案要修的假綠燈是同一種東西。
func sdNotify(msg string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return // 不是 systemd 起的，正常情況
	}
	if sock[0] == '@' { // abstract namespace
		sock = "\x00" + sock[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		log.Printf("sd_notify(%s) 連不上 %q：%v", msg, os.Getenv("NOTIFY_SOCKET"), err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(msg)); err != nil {
		log.Printf("sd_notify(%s) 寫入失敗：%v", msg, err)
	}
}

func notifyReady()    { sdNotify("READY=1") }
func notifyWatchdog() { sdNotify("WATCHDOG=1") }

// ---------------------------------------------------------------- 對帳迴圈

// reconcileLoop 定期重算每一台的狀態，把「變了」的那些寫進歷史。
//
// ⚠ 為什麼需要一個迴圈，而不是在收心跳時算就好：因為最重要的狀態轉移
// 「Online → Unreachable」是**沒有事件**的。一台機器安靜下來的時候，
// 不會有任何請求打進來觸發判斷。只靠事件驅動，一台死掉的機器會永遠
// 停在最後一次心跳時的 Online —— 而那就是這個產品要解決的那個 bug。
func (h *hub) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	prev := time.Now()
	for {
		start := time.Now()
		h.reconcile()
		now := time.Now()
		// ⚠ 兩個時鐘：now.Sub(prev) 走單調時鐘（程序有在跑的時間），
		// Round(0) 剝掉單調讀數之後走牆上時鐘（世界過了多久）。
		// 兩者的差就是「程序沒在跑、世界在走」—— 主機睡著或被暫停。
		for _, g := range hubLoopGaps(now.Round(0).Sub(prev.Round(0)), now.Sub(prev), now.Sub(start)) {
			h.journal(g.kind, g.detail, now)
		}
		prev = now
		if err := h.store.TouchHubAlive(now); err != nil {
			log.Printf("蓋不了「還活著」的章：%v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *hub) reconcile() {
	if err := h.store.ReconcileFleet(time.Now().UTC()); err != nil {
		log.Printf("對帳失敗：%v", err)
	}
}

// ---------------------------------------------------------------- 清舊資料

// pruneLoop 一天清一次舊資料。
//
// ⚠ 「今天清過了沒有」的依據是 retention_log 裡最後一筆的時間，不是一個
// 檔案戳章、也不是行程內的變數。理由有兩個，而且都是實測踩過的：
//
//   - 一個行程內的變數會在每次重啟時歸零。Hub 一天重啟六次就清六次。
//   - 一個外部戳章檔會跟資料庫**分開遺失**：檔案還在但資料庫換了一份，
//     它會說「清過了」；資料庫還在但檔案掉了，它會多清一次。
//     retention_log 跟被清的資料在同一個檔案裡，兩者不可能各自走散。
//
// ⚠ 排程刻意**不挑固定時刻**。早報是 08:00 因為人要在那個時間看到它；
// prune 沒有人在等，挑一個固定時刻只會讓它跟早報、備份、reconcile 撞在一起。
func (h *hub) pruneLoop(ctx context.Context) {
	// 起來先等一下。一個開機就開始刪東西的服務，會讓「重啟一下看看」
	// 變成一個有副作用的動作 —— 而那是排查時最常做的第一件事。
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Minute):
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		h.maybePrune(time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pruneInterval 是兩次清理之間至少要隔多久。
//
// ⚠ 20 小時而不是 24：用 24 的話，每天的清理時間會被前一天的執行時刻
// 往後推一點，一個月後就漂到別的時段去了。
const pruneInterval = 20 * time.Hour

func (h *hub) maybePrune(now time.Time) {
	last, _, ok, err := h.store.LastPrune()
	if err != nil {
		log.Printf("清理：讀不到上次的清理紀錄，這一輪跳過：%v", err)
		return
	}
	if ok && now.Sub(last) < pruneInterval {
		return
	}
	rep, err := h.store.Prune(now, h.retention, false)
	if err != nil {
		// ⚠ 不 Fatal。清不掉舊資料是「磁碟遲早會滿」，
		// 而直接讓 Hub 死掉是「現在就什麼都看不到」。後者嚴重得多。
		log.Printf("清理失敗：%v", err)
		return
	}
	// ⚠ 一定要印，連 0 也要印。一個從來不出聲的清理程序，
	// 沒有人分得出它是「沒東西要清」還是「三個月前就壞了」。
	log.Printf("清理：刪掉 %d 列，留下 %d 列各組最新的（%s）",
		rep.Total(), rep.KeptNewest, pruneSummary(rep))
}

func pruneSummary(rep store.PruneReport) string {
	parts := make([]string, 0, len(rep.Counts))
	for _, c := range rep.Counts {
		parts = append(parts, fmt.Sprintf("%s=%d", c.Table, c.Deleted))
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------- 早報

func (h *hub) reportLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			h.sweepDiskCleanAlerts(now)
			h.maybeSendReport(now)
		}
	}
}

// maybeSendReport 一天送一次，而且是「今天還沒成功送過」才送。
//
// ⚠ 判斷依據是 LastNotification（只算 delivered=1），不是「上次嘗試」。
// 把送失敗當成送過，會讓一整天安靜無聲 —— 而安靜在這個產品裡的意思是
// 「Hub 死了」。寧可重試到成功，也不要讓一則沒送到的早報吃掉當天的名額。
//
// 失敗之後的下一次嘗試在 maybeSendReport，不在 deliver：
// lastFailed + min(1m * 2^(failuresSinceDue-1), 60m)。次數與上一筆失敗時間
// 都從 notifications 讀，重啟不會把間隔忘掉。沒有設定推播時一天只記一列。
func (h *hub) maybeSendReport(now time.Time) {
	due, ok := h.reportDue(now)
	if !ok || now.Before(due) {
		return
	}
	last, found, err := h.store.LastNotification("daily")
	if err != nil {
		log.Printf("查上次推播失敗：%v", err)
		return
	}
	if found && !last.Before(due) {
		return // 今天已經送成功過了
	}
	rows, failures, lastFailed, err := h.store.NotificationAttemptsSince("daily", due)
	if err != nil {
		log.Printf("查早報嘗試失敗：%v", err)
		return
	}
	if h.notifier() == nil {
		if rows > 0 {
			return
		}
	} else if failures > 0 && !lastFailed.IsZero() {
		next := lastFailed.Add(notifyBackoff(failures))
		if now.Before(next) {
			return
		}
	}

	since := last
	if !found {
		since = now.Add(-24 * time.Hour)
	}
	body, err := h.buildReport(now, since)
	if err != nil {
		log.Printf("產生早報失敗：%v", err)
		return
	}
	h.deliver("daily", body, now)
}

// publicBase returns the one authority supported by the direct-tailnet
// operator topology. Reports, browser forms and generated agent enrollment
// commands must all name the same literal listener; accepting a second public
// hostname here would produce links that the Host-pinning boundary rejects.
//
// ⚠ 回傳的第二個值是「為什麼沒有」。它不是錯誤 —— 沒有連結是完全可以
// 接受的狀態，但**沒有理由地沒有連結**不行：那會變成一個沒有人發現
// 它壞掉的功能。
//
// CLAWCTL_PUBLIC_URL is retained only as a compatibility assertion: when set,
// it must equal the derived HTTP URL. Supporting TLS termination or a named
// origin later requires an explicit trusted-authority allowlist first.
func publicBase(listen string) (base, why string) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Sprintf("看不懂監聽位址 %q", listen)
	}
	ip, err := netip.ParseAddr(host)
	switch {
	case host == "" || err == nil && ip.IsUnspecified():
		return "", "監聽在所有介面上，推不出對外該用哪一個位址"
	case err != nil:
		return "", fmt.Sprintf("監聽位址 %q 不是 literal IP", host)
	case ip.IsLoopback():
		return "", "只監聽 loopback，沒有別台連得到的位址"
	case !tsaddr.IsTailscaleIP(ip.Unmap()):
		return "", fmt.Sprintf("監聽位址 %q 不是 Tailscale IP", host)
	}
	endpoint, err := operatorendpoint.ParseListen(net.JoinHostPort(host, port))
	if err != nil {
		return "", fmt.Sprintf("監聽位址 %q 不是受支援的 canonical Tailscale node endpoint：%v", listen, err)
	}
	base = endpoint.BaseURL()
	if configured := strings.TrimRight(os.Getenv("CLAWCTL_PUBLIC_URL"), "/"); configured != "" && configured != base {
		// Do not echo the configured value: environment files also hold bearer
		// material, and a mistaken URL can itself contain credentials.
		return "", fmt.Sprintf("CLAWCTL_PUBLIC_URL 與釘住的 operator authority %q 不一致；目前不支援另一個 hostname 或 TLS origin", base)
	}
	return base, ""
}

func (h *hub) buildReport(now, since time.Time) (string, error) {
	ov, err := h.store.Overview(now.UTC())
	if err != nil {
		return "", err
	}
	in := report.Input{
		Now:            now,
		Since:          since,
		FleetCounts:    ov.DenominatorCounts(),
		Expected:       ov.Expected,
		ReportingKnown: true,
		BaseURL:        h.publicURL,
	}
	in.Reporting = ov.ReportingCount()
	// ⚠⚠ 真的失聯時刻，不是 StateSince。
	//
	// StateSince 講的是「現在這個狀態什麼時候開始的」——只有在失聯剛好是
	// 這台機器最後發生的一件事時，它才等於失聯時刻。實機上 sampleagent4 的
	// StateSince 指著 00:46 那次眨眼、sampleagent2 的指著 08:17 那次，而它們在
	// 00:46 明明是同秒一起失聯的。差七個半小時，永遠併不起來。
	// 見 report.Machine.UnreachableAt。
	outages := map[string][]time.Time{}
	if chs, err := h.store.ChangesSince(since, now); err != nil {
		// ⚠ 讀不到就要出聲。靜靜地當作「沒有同時失聯」正是這一段在修的那個病：
		// 一個空的、乾淨的答案跟一個沒問對地方的答案長得一模一樣。
		log.Printf("讀取狀態轉移失敗，這次早報無法判斷同時失聯：%v", err)
	} else {
		for _, c := range chs {
			if c.Kind == "state" && state.State(c.To) == state.Unreachable {
				outages[c.MachineID] = append(outages[c.MachineID], c.At)
			}
		}
	}
	// 還原演練的章：從來沒蓋過、或太久沒蓋，早報都要催（PHASES D 表）。
	if h.drillStamp != "" {
		in.RestoreDrill.Tracked = true
		in.RestoreDrill.At, in.RestoreDrill.Done = readDrillStamp(h.drillStamp)
	}
	// Hub 自己的日誌：早報要拿它對「同時失聯」。窗口往前多拉一小時，
	// 因為上一則早報之前的一次重啟，也可能是這一輪窗口開頭那次眨眼的成因。
	if evs, err := h.store.HubEventsBetween(since.Add(-time.Hour), now); err != nil {
		log.Printf("讀取 Hub 自己的日誌失敗，這次早報無法解釋同時失聯：%v", err)
	} else {
		for _, e := range evs {
			in.HubEvents = append(in.HubEvents, report.HubEvent{At: e.At, Kind: e.Kind, Detail: e.Detail})
		}
	}
	for _, m := range ov.Machines {
		if !m.InDenominator() {
			continue
		}
		rm := report.Machine{
			ID:            m.MachineID,
			DisplayName:   m.DisplayName,
			UnreachableAt: outages[m.MachineID],
			State:         m.State,
			Reason:        m.Reason,
			Findings:      m.Findings,
		}
		if m.StateSince != nil {
			rm.StateSince = *m.StateSince
			rm.Changed = m.StateSince.After(since)
		}
		if rm.Changed {
			rm.PreviousState = h.previousState(m.MachineID)
		}
		for _, c := range m.Facts.Credentials {
			// ⚠ 還在寬限內的過期票不進早報。早報答的是「今天要動什麼手」，
			// 而一張閒置了 2 小時、下次用就會自己續的 claude 票沒有手要動 ——
			// 讓它天天上早報，就是 SPEC §4.3 刪掉 expires_soon 的那個理由。
			// 判斷用同一支 state.CredGrace，跟詳細頁的判決不會各說各話。
			if _, _, inGrace := state.CredGrace(c, m.Facts.Now); inGrace {
				continue
			}
			if c.Status == "expired" || c.Status == "expires_soon" || c.Status == "failed" {
				rm.CredExpiries = append(rm.CredExpiries, report.CredExpiry{
					Provider: c.Provider, Status: c.Status, ExpiresAt: c.ExpiresAt, Peers: c.Peers,
				})
			}
		}
		in.Machines = append(in.Machines, rm)
	}
	return report.Render(in), nil
}

// previousState 找上一段狀態。⚠ 不假設 History 的排序方向 ——
// 取「已經結束的區間裡最晚開始的那一段」，正著反著都對。
func (h *hub) previousState(machineID string) state.State {
	d, err := h.store.Detail(machineID, time.Now().UTC())
	if err != nil {
		return ""
	}
	var best store.StateSpan
	for _, s := range d.History {
		if s.LeftAt == nil {
			continue // 還開著的那段是「現在」，不是「上一個」
		}
		if best.State == "" || s.EnteredAt.After(best.EnteredAt) {
			best = s
		}
	}
	return best.State
}

// touchReportStamp 蓋章：一則 daily 早報確實離開了這台機器。
//
// ⚠ 只有 daily。臨時通知（未來的即時告警之類）不准碰這個檔 ——
// 否則一則半夜的告警會把時間戳刷新，而隔天早上早報根本沒送出去這件事
// 就被蓋掉了。死人之鐘watch 的是「每天那一則」，不是「有沒有任何東西送出去」。
//
// ⚠ 寫失敗只記 log 不影響早報。早報已經送到了，那是事實；
// 蓋不了章是另一件事，而它會由死人之鐘自己發現（時間戳變舊 → 告警）。
// 這裡如果因為寫檔失敗就把早報標成未送達，會製造一個假的壞消息。
func (h *hub) touchReportStamp(kind string, now time.Time) {
	if h.reportStamp == "" || kind != "daily" {
		return
	}
	// 先寫暫存檔再 rename：讀的那一端永遠看到完整的數字，不會撞上寫到一半。
	tmp := h.reportStamp + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(now.Unix(), 10)+"\n"), 0o644); err != nil {
		log.Printf("寫不了早報時間戳 %s：%v（早報本身已送達）", h.reportStamp, err)
		return
	}
	if err := os.Rename(tmp, h.reportStamp); err != nil {
		log.Printf("換不了早報時間戳 %s：%v（早報本身已送達）", h.reportStamp, err)
	}
}

// ---------------------------------------------------------------- 子指令

func cmdEnrollToken(argv []string) {
	if err := runEnrollTokenCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func cmdMachines(argv []string) {
	if err := runMachinesCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func cmdAudit(argv []string) {
	if err := runAuditCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func cmdRetire(argv []string) {
	translated, err := retireAliasArgs(argv)
	if err != nil {
		log.Fatal(err)
	}
	if err := runMachineCommand(context.Background(), translated, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func retireAliasArgs(argv []string) ([]string, error) {
	for _, arg := range argv {
		if arg == "--set" || arg == "-set" || strings.HasPrefix(arg, "--set=") || strings.HasPrefix(arg, "-set=") {
			return nil, errors.New("retire alias 不接受 --set；請改用 clawctl-hub machine lifecycle 明示 desired state")
		}
	}
	return append([]string{"lifecycle", "--set", "retired"}, argv...), nil
}

// cmdPrune 手動清一次舊資料。
//
// ⚠ 預設是 --dry-run。這支指令是這整個 CLI 裡唯一會**永久刪掉**東西的，
// 而 retire 那個危險得多的動作在網頁上要打字確認。一個手滑就刪掉三十天前
// 所有觀測的指令，預設值只能是「只看不動」。
//
// 這跟網頁上那條「摩擦力要不對稱」的規則是同一條：可以反悔的方向不設路障，
// 不能反悔的方向要。而刪資料是這個專案裡最不能反悔的方向 ——
// retire 可以取消、token 可以重開，被刪掉的觀測沒有任何辦法回來。
func cmdPrune(argv []string) {
	if err := runPruneCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

// parseAsOf 收 `2026-10-01` 或完整的 RFC3339。
//
// ⚠ 只給日期的時候補的是 00:00 **UTC**，不是本地時間。prune 圈的每一個
// 時間欄都是 Hub 自己的 UTC 鐘（見 pruneJob.cutCol 那段），這裡跟著走，
// 免得「我輸入 10-01，它卻用 09-30 16:00 去算」這種只差幾小時、
// 但剛好會讓答案差一整批的誤會。
//
// ⚠ 不接受相對時間（`+30d`）。這個旗標的輸出會被貼進工單跟文件裡，
// 而一個相對時間在三天後重讀時意思就變了。
func parseAsOf(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%q 不是 YYYY-MM-DD 也不是 RFC3339（例如 2026-10-01 或 "+
				"2026-10-01T00:00:00Z）", s)
	}
	return t.UTC(), nil
}

// loadExpectations 讀期望設定並交給 store。
//
// ⚠⚠ 這支存在的理由是**每一條會產生判決的路徑都必須走同一段**。
// 原本它只寫在 serve() 裡，於是 `clawctl-hub report` 預覽在**一條期望
// 都沒有**的世界裡重算了一次判決。而 cmdReport 的價值全在於
// 「它印出來的，就是早上八點會送出去的那一則」——
// 一個會說謊的預覽，比沒有預覽更糟。
//
// 2026-09-04 實測（`--since 1h`，兩次只差 CLAWCTL_EXPECTATIONS 這一個變數）：
//
//	沒期望：+2 more in hub
//	有期望：+3 more in hub   ← 多出來的那一行就是 sampleagent2 的事件流判決
//
// ⚠ 我第一次寫這段註解時說的是「同一台機器被講成另一個問題」，
// 還附了兩個看起來很有力的理由字串。**那是錯的** —— 見 PHASE1.md §5.26 續。
// 兩邊的主因其實都是憑證過期（promote 同分後到者覆蓋，而 3e 憑證
// 排在 3d2 產出物後面）。真正的影響是**少一行**。
// 少一行已經足夠嚴重，但**拿一個假的證據去支持一個真的 bug**，
// 下一個人照著那段註解去查會查不到，然後開始懷疑對的東西。
func loadExpectations(st *store.Store) *expect.Set {
	// ⚠⚠ 期望設定讀壞了要**大聲**，而且要在啟動時就講。
	// 安靜地跑下去會得到一台「所有期望都不見了」的 Hub，
	// 而畫面上那會長得跟「一切正常」一模一樣。
	exps := expect.Load(os.Getenv("CLAWCTL_EXPECTATIONS"))
	switch {
	case exps.Err != "":
		log.Printf("期望設定失敗：%s；rules=0", exps.Err)
	case !exps.Configured:
		log.Printf("expectations rules=0")
	default:
		log.Printf("載入 %d 條期望（%s）", len(exps.Rules), exps.Path)
	}
	st.SetExpectations(exps)
	return exps
}

// ---------------------------------------------------------------- 小工具

// openServeStore is called only while serve owns the process-lifetime writer
// lock.  An existing regular SQLite file is not sufficient identity: prove it
// has a complete, historically reachable clawctl schema before Store.Open can
// enable WAL or run migrations.  The post-open checks are cooperative-race
// assertions; the private state directory and writer protocol forbid sibling
// processes from renaming these paths while the lock is held.
func openServeStore(path string) (*store.Store, error) {
	var before os.FileInfo
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("資料庫 %s 不是 regular file", path)
		}
		if err := store.ValidateExistingLedger(path); err != nil {
			return nil, fmt.Errorf("既有檔案的 clawctl ledger pre-open 驗證失敗：%w", err)
		}
		before = info
	case errors.Is(err, os.ErrNotExist):
		// A genuinely absent target is the one explicit first-install case.
	default:
		return nil, fmt.Errorf("確認資料庫 %s 是否存在：%w", path, err)
	}

	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*store.Store, error) {
		return nil, errors.Join(err, st.Close())
	}
	after, err := os.Lstat(path)
	if err != nil {
		return fail(fmt.Errorf("writable open 後重查 ledger identity：%w", err))
	}
	if !after.Mode().IsRegular() || (before != nil && !os.SameFile(before, after)) {
		return fail(errors.New("ledger path 在驗證與 writable open 之間改變"))
	}
	if err := ledgerlock.ValidateUpgradeTarget(path); err != nil {
		return fail(fmt.Errorf("writable open 後 ledger path/sidecar identity 拒絕：%w", err))
	}
	if err := store.ValidateExistingLedger(path); err != nil {
		return fail(fmt.Errorf("migration 後 clawctl ledger identity 拒絕：%w", err))
	}
	return st, nil
}

// openExisting 開一個**必須已經存在**的資料庫。
//
// ⚠ 2026-09-03：`clawctl-hub machines` 印出「名冊上 0 台，0 台正在回報。」
// 而那台機器名冊上有 5 台、4 台正在回報。原因是 unit 檔寫 `--db …/clawctl.sqlite`、
// CLI 預設是 `…/hub.sqlite`，兩個檔案各自看都合理。
//
// 最糟的不是路徑不一致，是 store.Open 會**把檔案建出來** ——
// 於是那句「0 台」對那個剛出生的空 DB 而言完全正確。
// 一個自洽、自己生產證據、而且完全錯誤的答案，還在磁碟上留下一個誘餌檔，
// 讓下一個人看到兩個 .sqlite 分不出哪個是真的。
//
// 「名冊上 N 台」是這個產品最重要的一句話。它不准在讀錯檔案的時候還講得出來。
func openExisting(path string) (*store.Store, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("資料庫 %s 無法開啟：%v；檢查：systemctl --user cat clawctl-hub", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("資料庫 %s 不是 non-symlink regular file；拒絕 writable open", path)
	}
	if err := store.ValidateExistingLedger(path); err != nil {
		return nil, fmt.Errorf("資料庫 %s 的 clawctl ledger pre-open 驗證失敗：%w", path, err)
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		closeErr := st.Close()
		if err == nil {
			err = errors.New("path/inode changed")
		}
		return nil, errors.Join(fmt.Errorf("資料庫 %s 在驗證與 writable open 之間改變：%w", path, err), closeErr)
	}
	if err := store.ValidateExistingLedger(path); err != nil {
		return nil, errors.Join(fmt.Errorf("資料庫 %s migration 後 identity 拒絕：%w", path, err), st.Close())
	}
	return st, nil
}

func defaultDB() string {
	if v := os.Getenv("CLAWCTL_DB"); v != "" {
		return v
	}
	// CLI defaults to the same ledger path pinned by the service unit. The unit
	// repeats it intentionally: its explicit --db must outrank CLAWCTL_DB from
	// EnvironmentFile/user-manager ambient state so upgrade-hub.sh cannot
	// snapshot one ledger while the live writer migrated another. The cross-file
	// contract is executable in cmd/clawctl-hub/dbpath_test.go.
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.local/share/clawctl/clawctl.sqlite"
	}
	return "clawctl.sqlite"
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

func parseHM(s string) (int, int, bool) {
	var hh, mm int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &hh, &mm); err != nil {
		return 0, 0, false
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}
