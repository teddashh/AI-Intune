// clawctl-agent —— 住在被管機器上的觀測 agent。
//
// 設計約束（docs/SPEC.md §2）：
//
//   - 只做 outbound HTTP client traffic；目前部署在 Tailscale/WireGuard
//     已認證加密的隧道內。機器上不開任何 inbound 控制 port。
//   - 只回報事實，不做判決。它從不說自己健不健康。
//   - Hub 掛掉不影響它。連不上就重試，本機該量的照量。
//   - 跑在使用者身分下（systemd --user），不是 root ——
//     coding CLI 的憑證是 0600 使用者所有，而且每台機器的 user 都不一樣。
//   - 心跳與觀測分成兩條 goroutine。觀測那條卡住時，心跳仍要能送出
//     「我還在，但我卡住了」—— 否則 Hub 分不出「機器死了」與「觀測壞了」。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/teddashh/AI-Intune/internal/agenthub"
	"github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/probe"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

var version = "dev"

type config struct {
	HubURL                   string `json:"hub_url"`
	MachineID                string `json:"machine_id"`
	AgentToken               string `json:"agent_token"`
	EnrollmentSchemaVersion  int    `json:"enrollment_schema_version"`
	EnrollmentSettingsDigest string `json:"enrollment_settings_digest"`
	JobsEnabled              bool   `json:"jobs_enabled"`
	JobsPollIntervalSeconds  int    `json:"jobs_poll_interval_seconds"`

	// 間隔由 Hub 指派並在每次 check-in 回應裡更新，Agent 不自己決定 ——
	// 這樣要調整節奏不必重裝 30 台上的 agent。
	CheckinIntervalSeconds     int `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int `json:"observation_interval_seconds"`
}

// appliedSettings 是這個 process 此刻真的照著跑的節奏，以及它的 digest。
// 三個值必須一起換：回報 digest A 卻用 B 的間隔在跑，Hub 就無法知道機器
// 實際的節奏。
type appliedSettings struct {
	checkin     time.Duration
	observation time.Duration
	digest      string
}

// workloadPolicyBundle 必須以一個 atomic pointer 整包交換。若 rules 與 token
// 分開存，checkin 恰好在兩次 Load 中間更新時，Agent 會把 A 規則的量測標成
// B token，Hub 就無法知道這批證據實際用了哪一份政策。
type workloadPolicyBundle struct {
	expectations []model.Expectation
	token        string
}

func configPath() string {
	if p := os.Getenv("CLAWCTL_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "clawctl", "agent.json")
}

// statePath 是本機狀態的落地位置。存在的理由是重啟後還記得 agent_seq，
// 以及讓「上次觀測是什麼時候」有個檔案 mtime 可以問。
func statePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(dir, "clawctl", "agent-state.json")
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("clawctl-agent: ")

	// ⚠⚠ 這裡刻意沒有 default: runAgent()。每一條路都要指名 ——
	// 「掉下去就跑 daemon」正是 unknownArgError 記的那個六小時故障。
	switch dispatch(os.Args[1:]) {
	case actAgent:
		runAgent()
	case actEnroll:
		runEnroll(os.Args[2:])
	case actProbe:
		runProbeOnce(os.Args[2:])
	case actVerify:
		runVerify(os.Args[2:])
	case actVerifier:
		runVerifier(os.Args[2:])
	case actVersion:
		fmt.Println(version)
	case actHelp:
		fmt.Print(usage)
	default:
		log.Fatal(unknownArgError(os.Args[1]))
	}
}

// dispatch 認得的動作。⚠ actUnknown 不是一個「大概是 daemon」的預設值，
// 它是一個必須讓程式停下來的答案。
const (
	actAgent  = "agent"
	actEnroll = "enroll"
	actProbe  = "probe"
	actVerify = "verify"
	// actVerifier 是第二雙眼睛那一側，跟 actVerify（等 Hub 確認自己開始接單）
	// 沒有關係。兩個名字很像，做的事完全不同：一個問「Hub 看到我了嗎」，
	// 一個去看別台機器現在實際長怎樣。
	actVerifier = "verifier"
	actVersion  = "version"
	actHelp     = "help"
	actUnknown  = "unknown"
)

// dispatch 決定第一個參數要做什麼。
//
// ⚠⚠ 抽成一個純函式，是為了讓「認不得的參數不准變成 daemon」這件事
// 有一支測試能釘住。它原本是 main 裡一個沒有 default 的 switch，
// 而那個缺席的 default 就是那六個小時（見 unknownArgError）。
func dispatch(args []string) string {
	if len(args) == 0 {
		return actAgent // 沒有參數才是 daemon —— 那是 systemd 用的那一條
	}
	switch args[0] {
	case "enroll":
		return actEnroll
	case "probe":
		return actProbe
	case "verify":
		return actVerify
	case "verifier":
		return actVerifier
	case "version", "--version", "-v":
		return actVersion
	case "help", "--help", "-h":
		return actHelp
	}
	return actUnknown
}

const usage = `clawctl-agent —— 觀測這一台機器並回報給 Hub。

用法：
  clawctl-agent                             以 daemon 跑（systemd --user 用的是這個）
  clawctl-agent enroll --hub URL --token T  用一次性的票報到
  clawctl-agent probe                       印出一次觀測就結束，不回報
  clawctl-agent verify                      等 Hub 確認這個版本已開始接單
  clawctl-agent verifier --hub URL \
      --token-file T --targets F            以獨立 verifier 的身分，驗 Hub 指名給它的工作單
  clawctl-agent version                     印出版本
`

// unknownArgError 是「認不得的第一個參數」。
//
// ⚠⚠ 這段程式存在的理由是一個實測到的六小時故障（2026-09-03，sampleagent2）：
// 一支部署腳本打了 `clawctl-agent --version`，而這個 binary 的版本子命令是
// `version`，沒有 `--version`。舊的 switch 沒有 default，於是認不得的參數
// **安靜地掉下去、把 agent 跑起來了**。
//
// 結果那台機器上同時有兩個 agent：一個是 systemd 起的新版，一個是那行指令
// 起的舊版（binary 已經被換掉，但它記憶體裡是舊的）。兩個都在回報，
// 於是同一個工具在資料庫裡在兩個答案之間跳：
//
//	19:31  agy present=false   ← 舊版：查的是 daemon 的 PATH
//	19:38  agy present=true    ← 新版：查的是登入的 PATH
//	19:41  agy present=false
//
// 而**每一盞燈都是綠的** —— 兩個 agent 都在送心跳，那台機器看起來比別台
// 還要健康。一個在兩個真相之間跳動、而且兩邊都「看起來很像資料」的欄位，
// 比一個紅燈難查得多。
//
// 一個會把不認識的指令解釋成「那就跑 daemon 吧」的 CLI，
// 等於在替使用者的打錯字啟動背景程序。
func unknownArgError(arg string) string {
	return fmt.Sprintf("認不得的參數 %q。\n%s", arg, usage)
}

// ---------------------------------------------------------------- enroll

func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	hub := fs.String("hub", "", "Hub base URL: http://100.x.y.z:8787 (Tailscale, same address as the operator UI) or https://hostname (experimental Cloudflare Tunnel, agent check-in only)")
	tok := fs.String("token", "", "one-time enrollment token")
	tokenFile := fs.String("token-file", "", "0600 file containing the one-time enrollment token")
	_ = fs.Parse(args)
	if *hub == "" || (*tok == "") == (*tokenFile == "") {
		log.Fatal("enroll 需要 --hub，並且只指定 --token 或 --token-file 其中一個")
	}
	token := *tok
	if *tokenFile != "" {
		var err error
		token, err = readSecretFile(*tokenFile)
		if err != nil {
			log.Fatalf("讀取 enroll token 失敗：%v", err)
		}
	}
	canonicalHub, err := agenthub.Parse(*hub)
	if err != nil {
		log.Fatalf("enroll --hub 不合法：%v", err)
	}
	*hub = canonicalHub

	obs, err := probe.Collect(context.Background())
	if err != nil {
		// ⚠ 觀測失敗不該擋住報到。一台量不到東西的機器仍然應該出現在名冊裡 ——
		// 那正是我們最想看到的那種機器。
		log.Printf("報到前觀測失敗：%v", err)
	}

	req := model.EnrollRequest{
		SchemaVersion: model.SchemaVersion,
		EnrollToken:   token,
		Hostname:      obs.Identity.Hostname,
		MachineIDHint: obs.Identity.MachineIDHint,
		UnixUser:      obs.Identity.UnixUser,
		OS:            obs.Identity.OS,
		Arch:          obs.Identity.Arch,
		TailscaleIP:   obs.Identity.TailscaleIP,
		AgentVersion:  version,
	}

	var resp model.EnrollResponse
	if err := postJSONStrict(context.Background(), *hub+"/v1/enrollments", "", req, &resp); err != nil {
		log.Fatalf("報到失敗：%v", err)
	}

	cfg, err := enrollmentConfig(*hub, resp)
	if err != nil {
		log.Fatalf("報到失敗：Hub 回應不完整：%v", err)
	}
	if err := saveConfig(cfg); err != nil {
		log.Fatalf("寫入設定失敗：%v", err)
	}
	fmt.Printf("已報到。machine_id=%s\n設定寫在 %s（0600）\n", resp.MachineID, configPath())
}

func readSecretFile(path string) (string, error) {
	b, err := readPrivateRegularFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" || strings.ContainsAny(secret, "\r\n\t ") {
		return "", errors.New("secret file 內容格式不符")
	}
	return secret, nil
}

func readPrivateRegularFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("credential file 必須是 private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || after.Mode().Perm()&0o077 != 0 || !os.SameFile(before, after) {
		return nil, errors.New("credential file 在開啟時改變或不再 private")
	}
	return io.ReadAll(file)
}

func enrollmentConfig(hubURL string, resp model.EnrollResponse) (config, error) {
	var zero config
	if err := validateEnrollmentAuthority(resp.SchemaVersion, resp.MachineID, resp.AgentToken,
		resp.CheckinIntervalSeconds, resp.ObservationIntervalSeconds, resp.SettingsDigest); err != nil {
		return zero, err
	}
	return config{
		HubURL: hubURL, MachineID: resp.MachineID, AgentToken: resp.AgentToken,
		EnrollmentSchemaVersion:    resp.SchemaVersion,
		EnrollmentSettingsDigest:   resp.SettingsDigest,
		JobsEnabled:                true,
		CheckinIntervalSeconds:     resp.CheckinIntervalSeconds,
		ObservationIntervalSeconds: resp.ObservationIntervalSeconds,
	}, nil
}

func validateEnrollmentAuthority(schemaVersion int, machineID, agentToken string,
	checkinIntervalSeconds, observationIntervalSeconds int, settingsDigest string,
) error {
	if schemaVersion != model.SchemaVersion {
		return fmt.Errorf("schema_version=%d，預期 %d", schemaVersion, model.SchemaVersion)
	}
	if machineID == "" || machineID != strings.TrimSpace(machineID) || len(machineID) > 200 {
		return errors.New("缺少或不合法的 machine_id")
	}
	if agentToken == "" || agentToken != strings.TrimSpace(agentToken) ||
		strings.ContainsAny(agentToken, "\r\n\t ") || len(agentToken) > 200 {
		return errors.New("缺少或不合法的 agent_token")
	}
	wantDigest, err := assignedSettingsDigest(checkinIntervalSeconds, observationIntervalSeconds)
	if err != nil {
		return fmt.Errorf("Hub 沒有給有效的 check-in/observation interval：%w", err)
	}
	if settingsDigest != wantDigest {
		return errors.New("Hub 的 settings_digest 未綁定回應中的 interval")
	}
	return nil
}

func assignedSettingsDigest(checkinIntervalSeconds, observationIntervalSeconds int) (string, error) {
	return settingpolicy.Digest(settingpolicy.Settings{
		SchemaVersion:              settingpolicy.SchemaVersion,
		CheckinIntervalSeconds:     checkinIntervalSeconds,
		ObservationIntervalSeconds: observationIntervalSeconds,
	})
}

func runProbeOnce(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	pretty := fs.Bool("pretty", false, "indent output")
	_ = fs.Parse(args)

	obs, err := probe.Collect(context.Background())
	if err != nil {
		log.Printf("觀測有部分失敗（仍輸出已取得的部分）：%v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	if *pretty {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(obs)
}

func runVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	timeoutText := fs.String("timeout", "2m", "maximum time to wait for a Hub receipt")
	sinceText := fs.String("since", "", "minimum Hub receipt time in RFC3339")
	expectedHub := fs.String("hub", "", "expected Hub base URL")
	requirePlatformEvidence := fs.Bool("require-platform-evidence", false,
		"require fresh Hub-stored identity evidence matching this binary's platform")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		log.Fatal("verify 不接受位置參數")
	}
	timeout, err := time.ParseDuration(*timeoutText)
	if err != nil || timeout < time.Second || timeout > 10*time.Minute {
		log.Fatal("verify --timeout 必須介於 1s 與 10m")
	}
	var since time.Time
	if *sinceText != "" {
		since, err = time.Parse(time.RFC3339, *sinceText)
		if err != nil {
			log.Fatal("verify --since 必須是 RFC3339")
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("讀取設定失敗：%v", err)
	}
	if cfg.MachineID == "" {
		log.Fatal("設定缺少 machine_id")
	}
	if *expectedHub != "" && strings.TrimRight(*expectedHub, "/") != strings.TrimRight(cfg.HubURL, "/") {
		log.Fatal("設定的 Hub 與 --hub 不符")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	last := "Hub 尚未收到符合條件的 check-in"
	for {
		var receipt model.AgentReadinessResponse
		if err := doJSON(ctx, http.MethodGet, cfg.HubURL+"/v1/agent/readiness", cfg.AgentToken, nil, &receipt); err != nil {
			last = err.Error()
		} else if pending := readinessPending(receipt, cfg.MachineID, version, since, *requirePlatformEvidence); pending == "" {
			fmt.Printf("Hub ready: machine_id=%s checkin=%s jobs=enabled agent=%s",
				receipt.MachineID, receipt.LastCheckinReceivedAt.UTC().Format(time.RFC3339), receipt.AgentVersion)
			if *requirePlatformEvidence {
				fmt.Printf(" evidence=%s/%s received=%s", runtime.GOOS, runtime.GOARCH,
					receipt.IdentityReceivedAt.UTC().Format(time.RFC3339))
			}
			fmt.Println()
			return
		} else {
			last = pending
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Fatalf("Hub readiness 驗證逾時：%s", last)
		case <-timer.C:
		}
	}
}

func readinessPending(receipt model.AgentReadinessResponse, machineID, agentVersion string, since time.Time,
	requirePlatformEvidence bool,
) string {
	if receipt.MachineID != machineID {
		return "machine_id 不符"
	}
	if receipt.LastCheckinReceivedAt == nil {
		return "Hub 尚未收到 check-in"
	}
	if receipt.AgentStartedAt == nil || (!since.IsZero() && receipt.AgentStartedAt.Before(since)) {
		return "Hub 尚未收到本次 agent process 的 check-in"
	}
	if receipt.AgentVersion != agentVersion {
		return "Hub 收到的 agent 版本不符"
	}
	if receipt.JobsEnabled == nil || !*receipt.JobsEnabled {
		return "Hub 尚未確認 jobs enabled"
	}
	if receipt.DeviceSyncV1 == nil || !*receipt.DeviceSyncV1 {
		return "Hub 尚未確認 device-sync v1 executor"
	}
	if requirePlatformEvidence {
		// The installer cutoff and measurement use the agent's clock. The Hub
		// receipt proves storage, but its clock cannot establish agent freshness.
		if receipt.IdentityReceivedAt == nil || receipt.IdentityMeasuredAt == nil ||
			(!since.IsZero() && receipt.IdentityMeasuredAt.Before(since)) {
			return "Hub 尚未保存本次啟動後的 identity evidence"
		}
		platform, err := catalog.PlatformFromProbeIdentity(receipt.IdentityOS, receipt.IdentityArch)
		if err != nil {
			return "Hub 保存的 identity evidence 無法辨識平台"
		}
		if platform.OS != runtime.GOOS || platform.Arch != runtime.GOARCH {
			return fmt.Sprintf("Hub 保存的 identity evidence 平台不符：收到 %s/%s，需要 %s/%s",
				platform.OS, platform.Arch, runtime.GOOS, runtime.GOARCH)
		}
	}
	return ""
}

// ---------------------------------------------------------------- run

func runAgent() {
	applySoftMemoryLimit()
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("讀不到設定（跑過 `clawctl-agent enroll` 了嗎？）：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := loadState()
	// ⚠ boot 之後 seq 從上次接著跑，這樣 Hub 才看得出中間漏了幾顆心跳。
	//
	// ⚠⚠ 但正因為它跨重啟連續，它分辨不出 crash-loop —— 每 90 秒重啟一次的
	// agent，seq 看起來是平順遞增的。這裡原本的註解宣稱
	// 「seq 的連續性 + boot_id 的變化」能抓 crash-loop，兩個都不行：
	// boot_id 是機器的，process 重啟時它不動。真正在做這件事的是
	// AgentStartedAt。2026-09-03 那次全機隊團滅就是這樣被漏掉的。
	seq := &atomic.Int64{}
	seq.Store(st.AgentSeq)

	// 起手式是 agent.json 裡的值，配上上次採用時存下來的 digest。
	// 從未採用過任何 Hub 設定的機器 digest 是空的，Hub 會說「還沒回報」。
	var applied atomic.Pointer[appliedSettings]
	applied.Store(&appliedSettings{
		checkin:     dur(cfg.CheckinIntervalSeconds, 2*time.Minute),
		observation: dur(cfg.ObservationIntervalSeconds, 10*time.Minute),
		digest:      st.SettingsDigest,
	})

	obsAge := &atomic.Int64{} // 上次成功觀測距今幾秒，-1 = 從未
	obsAge.Store(-1)
	var lastObs atomic.Pointer[time.Time]
	// policy 是 Hub 下發的期望與其 opaque token；兩者必須原子地一起換。
	var policy atomic.Pointer[workloadPolicyBundle]

	// lastTurn：心跳迴圈上一圈開始的時刻。看門狗只認這個。
	// ⚠ 蓋章的時機是「進到這一圈」，不是「這一圈成功了」。Hub 不通不是這台的錯。
	lastTurn := &atomic.Int64{}
	lastTurn.Store(time.Now().UnixNano())

	// --- 看門狗 goroutine，有自己的時鐘。
	// ⚠ 只看心跳迴圈；觀測那條卡住時 unit 不該被打死 ——
	// 它應該繼續回報「我還在，但我卡在觀測」。
	if deadline := watchdogFromEnv(); deadline > 0 {
		// stall：心跳迴圈多久沒轉才算真的卡死。要大於最壞情況的 check-in 週期
		// （interval +20% 抖動），否則正常運轉會被誤判成卡死。
		stall := 3 * applied.Load().checkin
		go feedWatchdog(ctx, watchdogFeedInterval(deadline), stall, lastTurn, notifyWatchdog)
	}

	observationNudge := make(chan struct{}, 1)
	startJobs(ctx, cfg, observationNudge)

	// --- 心跳 goroutine。
	go func() {
		notifyReady()
		var skewTrack clockSkewTracker
		for {
			lastTurn.Store(time.Now().UnixNano())
			hb, err := probe.Heartbeat(ctx, seq.Add(1))
			if err != nil {
				log.Printf("心跳量測失敗：%v", err)
			}
			if t := lastObs.Load(); t != nil {
				age := int64(time.Since(*t).Seconds())
				hb.ObservationAgeSeconds = &age
			}
			hb.SchemaVersion = model.SchemaVersion
			hb.AgentVersion = version
			jobsEnabled := cfg.JobsEnabled
			hb.JobsEnabled = &jobsEnabled
			advertiseJobCapabilities(&hb)
			now := applied.Load()
			hb.SettingsDigest = now.digest

			var resp model.CheckinResponse
			// SentAt and the RTT sample share one clock reading so the skew
			// estimate can subtract half the postJSON round trip.
			callStart := time.Now()
			hb.SentAt = callStart.UTC()
			postErr := postJSON(ctx, cfg.HubURL+"/v1/checkins", cfg.AgentToken, hb, &resp)
			rtt := time.Since(callStart)
			if postErr == nil {
				now = adoptSettings(now, resp, &applied, cfg, seq)
				// ⚠ 期望由 Hub 下發，agent 不自己決定要量什麼。規則與 token
				// 是同一份證據的身分，必須用同一次 atomic Store 發布。
				bundle := &workloadPolicyBundle{
					expectations: append([]model.Expectation(nil), resp.Expectations...),
					token:        resp.WorkloadPolicyToken,
				}
				policy.Store(bundle)
				if skew, ok := estimateClockSkew(hb.SentAt, resp.ReceivedAt, rtt); ok {
					var event clockSkewEvent
					skewTrack, event = skewTrack.observe(time.Now(), skew, rtt)
					if line := formatClockSkewEvent(event); line != "" {
						log.Printf("%s", line)
					}
				}
			}
			wait := jitter(now.checkin)
			if postErr != nil {
				if honored, _, busy := hubBusyWait(wait, postErr); busy {
					// The systemd watchdog treats a heartbeat turn longer than
					// 3x the check-in interval as a hang, so a long Retry-After
					// is clamped to 2x the interval here.
					wait = min(honored, 2*now.checkin)
					logHubBusy(wait)
				} else {
					// ⚠ Hub 掛掉不是這台機器的錯，也不該讓 agent 停。只記錄，繼續。
					log.Printf("check-in 送不出去（會重試）：%v", postErr)
				}
			}
			saveState(agentState{AgentSeq: seq.Load(), SettingsDigest: now.digest})

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()

	// --- 觀測 goroutine：慢、重、允許失敗。
	// ⚠ 間隔每一圈重讀。以前它只在啟動時讀一次，於是 Hub 下發的觀測間隔
	// 實際上要等 agent 重啟才會生效 —— 設定看起來送到了，其實沒在跑。
	for {
		octx, ocancel := context.WithTimeout(ctx, 2*time.Minute)
		obs, err := probe.Collect(octx)
		ocancel()
		if err != nil {
			// ⚠ 部分失敗仍然要送。一台「claude 讀得到、openclaw 讀不到」的機器，
			// 它的 claude 狀態依然是有價值的事實。全有全無會讓最壞的機器
			// 剛好變成資料最少的機器。
			log.Printf("觀測部分失敗：%v", err)
		}
		obs.SchemaVersion = model.SchemaVersion
		obs.MeasuredAt = time.Now().UTC()

		// ⚠ 期望是 Hub 下發的，第一次 checkin 之前是 nil ——
		// 那時候送空的 Artifacts 是對的：我們**還不知道**要量什麼，
		// 而不是「量過了，沒有東西要量」。Hub 那邊分得出來（見 Detail.Expectations）。
		applyWorkloadPolicy(&obs, policy.Load())

		postErr := postJSON(ctx, cfg.HubURL+"/v1/observations:batch", cfg.AgentToken, obs, nil)
		if postErr == nil {
			now := time.Now()
			lastObs.Store(&now)
		}
		wait := jitter(applied.Load().observation)
		var floor time.Duration
		if postErr != nil {
			if honored, serverFloor, busy := hubBusyWait(wait, postErr); busy {
				wait = honored
				floor = serverFloor
				logHubBusy(wait)
			} else {
				log.Printf("觀測送不出去（會重試）：%v", postErr)
			}
		}

		if waitNextObservation(ctx, wait, floor, observationNudge) == observationCanceled {
			return
		}
	}
}

func advertiseJobCapabilities(checkin *model.Checkin) {
	checkin.DeviceSyncV1 = true
	checkin.MaintenanceDiskCleanV1 = true
}

// applyWorkloadPolicy 的 caller 只 Load atomic pointer 一次；這個函式用那個
// immutable bundle 同時量 artifact/event 並蓋 token，避免跨 policy 拼接。
func applyWorkloadPolicy(obs *model.ObservationBatch, policy *workloadPolicyBundle) {
	if policy == nil {
		return
	}
	obs.Artifacts = probe.CheckArtifacts(policy.expectations)
	// ⚠ 用 obs.MeasuredAt（agent 自己的時鐘）當事件窗口的原點。
	// 這裡跟 artifact 不一樣：artifact 只回報 mtime、由 Hub 用 Hub 的
	// 時鐘去減，而事件的窗口篩選一定得在讀檔的當下做。
	obs.Events = probe.CheckEvents(policy.expectations, obs.MeasuredAt)
	obs.WorkloadPolicyToken = policy.token
}

// ---------------------------------------------------------------- helpers

type observationWakeReason uint8

const (
	observationCanceled observationWakeReason = iota
	observationTimer
	observationNudged
)

// waitNextObservation waits for the caller-supplied duration or a job nudge.
// The caller already applied jitter. floor is the server Retry-After from a
// 503: a nudge may shorten the normal interval, but not this floor, so the next
// post is not earlier than the hub asked. A zero floor keeps the old race
// between the timer and a queued nudge.
//
// ⚠ nudge 不是把平常觀測調快，而是機器剛被工作單改過後，舊事實不准再掛
// 10 分鐘。channel 只有一格且送端不阻塞；滿了代表已經有一輪排隊，丟掉也沒關係。
func waitNextObservation(ctx context.Context, wait, floor time.Duration, nudge <-chan struct{}) observationWakeReason {
	if wait < 0 {
		wait = 0
	}
	if floor < 0 {
		floor = 0
	}
	if floor > wait {
		floor = wait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	if floor <= 0 {
		select {
		case <-ctx.Done():
			return observationCanceled
		case <-timer.C:
			return observationTimer
		case <-nudge:
			return observationNudged
		}
	}
	if floor >= wait {
		select {
		case <-ctx.Done():
			return observationCanceled
		case <-timer.C:
			return observationTimer
		}
	}
	floorTimer := time.NewTimer(floor)
	defer floorTimer.Stop()
	select {
	case <-ctx.Done():
		return observationCanceled
	case <-timer.C:
		return observationTimer
	case <-floorTimer.C:
	}
	select {
	case <-ctx.Done():
		return observationCanceled
	case <-timer.C:
		return observationTimer
	case <-nudge:
		return observationNudged
	}
}

// jitter 加 ±20% 的抖動。
//
// ⚠ 沒有這個，全機隊會在整分鐘同時打 Hub，而且重啟後會永遠對齊。
// 5 台還好，但這是那種你不做、之後就永遠不會想起來要做的事。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Minute
	}
	span := int64(d) * 2 / 10 // ±20%
	n, err := rand.Int(rand.Reader, big.NewInt(span*2))
	if err != nil {
		return d
	}
	return d + time.Duration(n.Int64()-span)
}

// adoptSettings takes the Hub's answer and, only when it names a different
// settings document, starts running it.
//
// ⚠ 採用之後才把新的 digest 當成「我在跑的那一份」。在同一圈裡先改 digest
// 再用舊間隔睡覺，Hub 收到的就會是一份它無法驗證的自述。
// ⚠ 間隔值一起寫回 agent.json：重啟之後要接著跑指派的節奏，不是退回註冊
// 當下的值。
func adoptSettings(current *appliedSettings, resp model.CheckinResponse,
	holder *atomic.Pointer[appliedSettings], cfg config, seq *atomic.Int64) *appliedSettings {
	wantDigest, err := assignedSettingsDigest(resp.CheckinIntervalSeconds, resp.ObservationIntervalSeconds)
	if err != nil || resp.SettingsDigest != wantDigest {
		log.Printf("拒絕未綁定 interval 的 Hub 設定回應")
		return current
	}
	next := &appliedSettings{
		checkin:     time.Duration(resp.CheckinIntervalSeconds) * time.Second,
		observation: time.Duration(resp.ObservationIntervalSeconds) * time.Second,
		digest:      resp.SettingsDigest,
	}
	if next.checkin == current.checkin && next.observation == current.observation &&
		next.digest == current.digest {
		return current
	}
	holder.Store(next)
	cfg.CheckinIntervalSeconds = resp.CheckinIntervalSeconds
	cfg.ObservationIntervalSeconds = resp.ObservationIntervalSeconds
	cfg.EnrollmentSettingsDigest = resp.SettingsDigest
	if err := saveConfig(cfg); err != nil {
		log.Printf("新設定已採用，但寫回 agent.json 失敗（重啟後會退回舊節奏）：%v", err)
	}
	saveState(agentState{AgentSeq: seq.Load(), SettingsDigest: next.digest})
	log.Printf("採用新設定：check-in %s、observation %s", next.checkin, next.observation)
	return next
}

func dur(seconds int, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func postJSON(ctx context.Context, url, token string, body, out any) error {
	return doJSON(ctx, http.MethodPost, url, token, body, out)
}

// postJSONStrict is reserved for authority-establishing responses. Enrollment
// enables the managed job executor, so an ambiguous or expanded 200 response
// must not be treated as a credential receipt.
func postJSONStrict(ctx context.Context, url, token string, body, out any) error {
	status, err := doJSONStatusMode(ctx, http.MethodPost, url, token, body, out, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("Hub authority receipt 回了未預期的 HTTP %d，需要 200", status)
	}
	return nil
}

// doJSON 是 Agent 所有 JSON HTTP 呼叫的共同入口。body=nil 時不送 request body。
func doJSON(ctx context.Context, method, url, token string, body, out any) error {
	_, err := doJSONStatus(ctx, method, url, token, body, out)
	return err
}

// doJSONStatus 另外交回 status，讓工作單協定能分清 200/201/202/204。
func doJSONStatus(ctx context.Context, method, url, token string, body, out any) (int, error) {
	return doJSONStatusMode(ctx, method, url, token, body, out, false)
}

func doJSONStatusMode(ctx context.Context, method, url, token string, body, out any, strict bool) (int, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr model.APIError
		_ = json.Unmarshal(b, &apiErr)
		httpErr := &hubHTTPError{StatusCode: resp.StatusCode, APIError: apiErr, Body: b}
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
			if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
				httpErr.RetryAfter = d
				httpErr.HasRetryAfter = true
			}
		}
		return resp.StatusCode, httpErr
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if strict {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
		if err != nil {
			return resp.StatusCode, err
		}
		if len(raw) > 1<<20 {
			return resp.StatusCode, errors.New("Hub JSON 回應超過 1 MiB")
		}
		if err := validateUniqueJSONFields(raw); err != nil {
			return resp.StatusCode, fmt.Errorf("Hub JSON 回應不是唯一欄位文件：%w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("Hub JSON 回應不符合 schema：%w", err)
		}
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

// hubHTTPError 保留協定層需要判斷的 status/code，也維持既有錯誤文字的形狀。
// RetryAfter is set only for 503 and 429 when Retry-After parsed. The duration
// is already capped. HasRetryAfter distinguishes a real zero (retry now) from
// a missing or garbage header.
type hubHTTPError struct {
	StatusCode    int
	APIError      model.APIError
	Body          []byte
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func (e *hubHTTPError) Error() string {
	if e.APIError.Code != "" {
		return fmt.Sprintf("hub %d %s: %s", e.StatusCode, e.APIError.Code, e.APIError.Message)
	}
	return fmt.Sprintf("hub %d: %s", e.StatusCode, bytes.TrimSpace(e.Body))
}

// ---------------------------------------------------------------- config I/O

func loadConfig() (config, error) {
	var c config
	b, err := readPrivateRegularFile(configPath())
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.HubURL == "" || c.HubURL != strings.TrimSpace(c.HubURL) {
		return c, errors.New("設定不完整：缺少或不合法的 hub_url")
	}
	if err := validateEnrollmentAuthority(c.EnrollmentSchemaVersion, c.MachineID, c.AgentToken,
		c.CheckinIntervalSeconds, c.ObservationIntervalSeconds, c.EnrollmentSettingsDigest); err != nil {
		return c, fmt.Errorf("設定中的 enrollment receipt 無效：%w", err)
	}
	return c, nil
}

func saveConfig(c config) error {
	if c.HubURL == "" || c.HubURL != strings.TrimSpace(c.HubURL) {
		return errors.New("設定不完整：缺少或不合法的 hub_url")
	}
	if err := validateEnrollmentAuthority(c.EnrollmentSchemaVersion, c.MachineID, c.AgentToken,
		c.CheckinIntervalSeconds, c.ObservationIntervalSeconds, c.EnrollmentSettingsDigest); err != nil {
		return fmt.Errorf("設定中的 enrollment receipt 無效：%w", err)
	}
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// ⚠ 0600。這個檔案裡有 agent token。
	return writePrivateFile(p, append(b, '\n'))
}

func writePrivateFile(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".clawctl-private-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

type agentState struct {
	AgentSeq int64 `json:"agent_seq"`
	// SettingsDigest 是這台機器此刻正在跑的設定文件 digest。跨重啟保存，
	// 因為重啟之後它跑的還是同一份（間隔值也一起寫回 agent.json）。
	//
	// ⚠ 只在真的採用了新設定時才寫。agent 回報的必須是它**正在跑**的那一份，
	// 不是 Hub 剛剛說它應該跑的那一份 —— 否則 Hub 問「套用了沒有」就是在問
	// 自己，答案永遠是「有」。
	SettingsDigest string `json:"settings_digest,omitempty"`
}

func loadState() agentState {
	var s agentState
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveState(s agentState) {
	p := statePath()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	if b, err := json.Marshal(s); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

// ---------------------------------------------------------------- sd_notify
//
// systemd 的 Type=notify 與 WatchdogSec 只需要往 $NOTIFY_SOCKET 送幾個字，
// 不值得為它引入一個相依。

func sdNotify(msg string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	if sock[0] == '@' { // abstract namespace
		sock = "\x00" + sock[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(msg))
}

func notifyReady()    { sdNotify("READY=1") }
func notifyWatchdog() { sdNotify("WATCHDOG=1") }

// ---------------------------------------------------------------- watchdog
//
// 2026-09-03：全機隊每 90 秒被 systemd SIGABRT 一次，從部署第一天起，
// 而 Hub 一路顯示綠燈 —— 因為每次重啟都會立刻送出一次 check-in。
// 「心跳」正是「它剛死過」的產物。
//
// 原因是兩個各自合理的數字放在兩個檔案裡：
//   ops/clawctl-agent.service  WatchdogSec=90
//   這裡                        餵食掛在 check-in 上，週期 120s ±20% = 96–144s
//
// 教訓寫成兩條規則，底下的程式碼就是這兩條：
//
//  1. 看門狗要有自己的時鐘，週期從 systemd 給的 WATCHDOG_USEC 推出來，
//     不准跟著任何一條業務迴圈的節奏走。業務節奏是會被調的（Hub 每次
//     check-in 都可以改 interval），看門狗的期限不會跟著改。
//  2. 餵食的憑據是「心跳迴圈確實轉過一圈」，不是「Hub 答應了」。
//     把 Hub 可達性接到看門狗上，等於 Hub 停 90 秒就團滅整個機隊。

// watchdogFromEnv 讀 systemd 設的 WATCHDOG_USEC。沒有就回 0（代表不啟用）。
func watchdogFromEnv() time.Duration {
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	return time.Duration(us) * time.Microsecond
}

// watchdogFeedInterval：期限的三分之一。
// ⚠ 不是二分之一 —— 二分之一表示掉一次封包就剛好踩線。三分之一容得下掉一次。
func watchdogFeedInterval(deadline time.Duration) time.Duration {
	if deadline <= 0 {
		return 0
	}
	return deadline / 3
}

// feedWatchdog 每 every 檢查一次心跳迴圈上次轉圈的時間；只要還在 stall 之內就餵。
// 超過 stall 就閉嘴，讓 systemd 把這個真的卡死的 process 打掉重來。
func feedWatchdog(ctx context.Context, every, stall time.Duration, lastTurn *atomic.Int64, ping func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ns := lastTurn.Load(); ns > 0 && time.Since(time.Unix(0, ns)) < stall {
				ping()
			}
		}
	}
}
