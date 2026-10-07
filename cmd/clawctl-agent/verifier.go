package main

// 第二雙眼睛的那一側。
//
// 這支子命令不是 agent 的一部分，只是共用同一個 binary：它用的是 verifier
// bearer（另一張表、另一組 hash），讀的是 operator 指名給它的工作單，
// 寫的是獨立證據。它永遠不領工作單、不續租、不回報終態。
//
// ⚠ 規則寫在這個檔案裡，不是 Hub 發下來的。Hub 只說「看這一張單」。
// 一個會執行 Hub 下發指令的 verifier 不是第二個判斷，
// 是一條用單一 bearer 控管的遠端執行管道。
//
// ⚠ 「量不到」不是一個判決。ssh 連不上、機器不在 targets 檔裡、systemctl
// 回不了話——這幾種都**不寫證據**，讓那張派工留在「已發出，等它回報」，
// 並以非零 exit 說出卡在哪裡。把量不到寫成 failed，等於用一台好機器的
// 網路問題去汙染升級判斷。

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	// verifierTargetsSchema 是 targets 檔唯一認得的版本。認不得就停，
	// 不猜——一份看得懂一半的機器清單會 ssh 到錯的地方去。
	verifierTargetsSchema = 1

	// verifierToolMissingExit 是遠端 shell 用來說「這台沒有這個工具」的碼。
	// 它與 curl（7/28）、systemctl（1/3/4）、readlink（1）都不重疊，
	// 所以看到它就知道是量不到，不是量到了壞消息。
	verifierToolMissingExit = 120

	// verifierSSHFailureExit 是 ssh 自己的傳輸失敗碼（連不上、user 錯、
	// host key 不符）。三條規則用的遠端指令都不會回這個值。
	verifierSSHFailureExit = 255

	verifierCommandTimeout = 30 * time.Second
	verifierGatewayPort    = 18789

	// 2026-09-12 與 2026-09-13 在 sampleagent4 各量到一次：unit 已經
	// active/running 之後，gateway 分別約 20 秒與 15 秒才開始回 200。
	// 只有這一種組合才進收斂窗；unit 沒跑仍立即留下失敗。
	verifierGatewaySettleTimeout = 30 * time.Second
	verifierGatewayRetryInterval = 5 * time.Second

	// maxVerifierCommandBytes 是 Hub 對 command 欄位的上限。超過會被整列退回，
	// 所以在這邊就截，而不是讓一張量好的單在送出時才死。
	maxVerifierCommandBytes = 16 << 10
)

// verifierTarget 說明怎麼走到一台機器。machine_id 是 Hub 的穩定鍵，
// ssh_destination 是這個 verifier 自己的事——Hub 不知道也不需要知道。
type verifierTarget struct {
	MachineID      string `json:"machine_id"`
	SSHDestination string `json:"ssh_destination"`
}

type verifierTargetsFile struct {
	SchemaVersion int              `json:"schema_version"`
	Targets       []verifierTarget `json:"targets"`
}

// sshResult 是一次遠端量測的原始結果。Transport 為 true 代表 ssh 自己
// 沒走到遠端，所以 ExitCode 與輸出都不是那台機器說的話。
type sshResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Transport bool
}

type sshRunFunc func(ctx context.Context, destination, command string) sshResult

// verifierJudgement 是一條規則的判讀。Observed 為 false 時 Passed 沒有意義，
// 呼叫端必須整張單都不寫。
type verifierJudgement struct {
	Observed        bool
	Passed          bool
	ObservedVersion string
	Summary         string
}

type verifierRule struct {
	ID      string
	Command string
	Judge   func(sshResult) verifierJudgement
}

// verifierRules 是這個 verifier 會問的全部問題。每一條都問「現在實際長怎樣」，
// 不問「剛才那道指令有沒有成功」——後者是 executor 已經回答過的問題。
//
// ⚠ 三條指令都是單行 POSIX sh，因為它們會原樣進證據；一個讀證據的人
// 必須能把那一行貼進自己的終端機重跑一次。
var verifierRules = []verifierRule{
	{
		ID: model.IndependentRuleOpenClawCurrentRelease,
		Command: `link=$(readlink -- "$HOME/.local/share/clawctl/openclaw/current" 2>/dev/null); ` +
			`printf 'current=%s\n' "$link"`,
		Judge: judgeCurrentRelease,
	},
	{
		ID: model.IndependentRuleOpenClawGatewayHTTP,
		Command: `command -v curl >/dev/null 2>&1 || exit 120; ` +
			`code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' ` +
			fmt.Sprintf("http://127.0.0.1:%d/health", verifierGatewayPort) +
			` 2>/dev/null); printf 'http_code=%s\n' "$code"`,
		Judge: judgeGatewayHTTP,
	},
	{
		ID: model.IndependentRuleOpenClawUnitState,
		Command: `command -v systemctl >/dev/null 2>&1 || exit 120; ` +
			`systemctl --user show ` + openClawUnit +
			` -p LoadState -p ActiveState -p SubState -p NRestarts`,
		Judge: judgeUnitState,
	},
}

// judgeCurrentRelease 讀的是 symlink 指到哪裡，不是任何程式對自己的說法。
// 空字串代表這台機器上沒有 clawctl 管的安裝——那是一個結論明確的觀測，
// 不是量不到。
func judgeCurrentRelease(res sshResult) verifierJudgement {
	if res.Transport || res.ExitCode != 0 {
		return verifierJudgement{}
	}
	props, ok := parseKeyValues(res.Stdout)
	if !ok {
		return verifierJudgement{}
	}
	link, ok := props["current"]
	if !ok {
		return verifierJudgement{}
	}
	if link == "" {
		return verifierJudgement{Observed: true, Summary: "current does not exist, machine has no clawctl-managed OpenClaw installation"}
	}
	version, ok := strings.CutPrefix(link, "releases/")
	if !ok || version == "" || strings.Contains(version, "/") {
		return verifierJudgement{Observed: true, Summary: "current points outside releases: " + link}
	}
	return verifierJudgement{Observed: true, Passed: true, ObservedVersion: version,
		Summary: "current=releases/" + version}
}

// judgeGatewayHTTP 只認「有沒有答」與「答的 status 是多少」。
// 它刻意不讀 body：gateway 說自己 ok 是自證，答得出 200 才是觀測。
func judgeGatewayHTTP(res sshResult) verifierJudgement {
	if res.Transport || res.ExitCode != 0 {
		return verifierJudgement{}
	}
	props, ok := parseKeyValues(res.Stdout)
	if !ok {
		return verifierJudgement{}
	}
	code, err := strconv.Atoi(props["http_code"])
	if err != nil || code < 0 || code > 999 {
		return verifierJudgement{}
	}
	if code == 0 {
		return verifierJudgement{Observed: true,
			Summary: fmt.Sprintf("127.0.0.1:%d did not respond", verifierGatewayPort)}
	}
	return verifierJudgement{Observed: true, Passed: code == http.StatusOK,
		Summary: fmt.Sprintf("http_code=%d", code)}
}

// judgeUnitState 讀 systemd 自己的 property 輸出，不讀 journal 文字。
func judgeUnitState(res sshResult) verifierJudgement {
	if res.Transport || res.ExitCode != 0 {
		return verifierJudgement{}
	}
	props, ok := parseKeyValues(res.Stdout)
	if !ok {
		return verifierJudgement{}
	}
	load, hasLoad := props["LoadState"]
	active, hasActive := props["ActiveState"]
	sub, hasSub := props["SubState"]
	if !hasLoad || !hasActive || !hasSub {
		return verifierJudgement{}
	}
	summary := fmt.Sprintf("%s／%s（%s", active, sub, load)
	if restarts, ok := props["NRestarts"]; ok {
		summary += "，restarts " + restarts
	}
	return verifierJudgement{Observed: true,
		Passed:  active == "active" && sub == "running",
		Summary: summary + "）"}
}

// parseKeyValues 把 key=value 逐行收成 map。同一個 key 出現兩個不同的值時
// 整批作廢：兩個答案的輸出不能被當成其中一個。
func parseKeyValues(stdout string) (map[string]string, bool) {
	props := make(map[string]string)
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, false
		}
		if seen, dup := props[key]; dup && seen != value {
			return nil, false
		}
		props[key] = value
	}
	return props, len(props) > 0
}

// ---------------------------------------------------------------- 子命令

func runVerifier(args []string) {
	fs := flag.NewFlagSet("verifier", flag.ExitOnError)
	hub := fs.String("hub", "", "Hub base URL, e.g. http://100.x.y.z:8787 over Tailscale")
	tokenFile := fs.String("token-file", "", "private file containing this verifier's bearer token")
	targetsPath := fs.String("targets", "", "JSON file mapping machine_id to an ssh destination")
	dryRun := fs.Bool("dry-run", false, "measure and print, send nothing to the Hub")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		log.Fatal("verifier does not accept positional arguments")
	}
	if *hub == "" || *tokenFile == "" || *targetsPath == "" {
		log.Fatal("verifier requires --hub, --token-file, and --targets")
	}
	token, err := readSecretFile(*tokenFile)
	if err != nil {
		log.Fatalf("failed to read verifier token: %v", err)
	}
	targets, err := loadVerifierTargets(*targetsPath)
	if err != nil {
		log.Fatalf("failed to read %s: %v", *targetsPath, err)
	}

	run := verifierRun{
		hubURL: strings.TrimRight(*hub, "/"), token: token,
		targets: targets, targetsPath: *targetsPath, dryRun: *dryRun,
		ssh: runSSH, now: func() time.Time { return time.Now().UTC() },
		wait: waitVerifierInterval,
		out:  os.Stdout,
	}
	if err := run.once(context.Background()); err != nil {
		log.Fatal(err)
	}
}

type verifierRun struct {
	hubURL      string
	token       string
	targets     map[string]string
	targetsPath string
	dryRun      bool
	ssh         sshRunFunc
	now         func() time.Time
	wait        func(context.Context, time.Duration) bool
	out         io.Writer
}

type verifierMeasurement struct {
	rule       verifierRule
	result     sshResult
	judgement  verifierJudgement
	verifiedAt time.Time
	settledFor time.Duration
}

func waitVerifierInterval(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// once 做完一輪就結束：讀指名給自己的單、量、回報。要它定期跑是 systemd
// 的事，不是這支程式的事——一個自己排程的 verifier 會多出一套與證據無關的
// 生命週期，而那正是派工表刻意不做的東西。
func (v verifierRun) once(ctx context.Context) error {
	var resp model.VerificationAssignmentsResponse
	if err := doJSON(ctx, http.MethodGet, v.hubURL+"/v1/verification-assignments", v.token, nil, &resp); err != nil {
		return fmt.Errorf("failed to read assignments: %w", err)
	}
	if resp.SchemaVersion != model.SchemaVersion {
		return fmt.Errorf("Hub returned schema_version=%d, this verifier only understands %d; please upgrade",
			resp.SchemaVersion, model.SchemaVersion)
	}
	if len(resp.Assignments) == 0 {
		fmt.Fprintln(v.out, "No jobs assigned to this verifier.")
		return nil
	}

	var stuck int
	for _, assignment := range resp.Assignments {
		if !v.report(ctx, assignment) {
			stuck++
		}
	}
	if stuck > 0 {
		return fmt.Errorf("%d assignments failed to submit evidence and remain in pending report; fix the reasons above and run again", stuck)
	}
	return nil
}

// report 量一張單的每一條規則，全部量得到才送。少一條就整張不送，因為
// 一張只送了一半的派工會從「等它回報」消失，看起來像做完了。
func (v verifierRun) report(ctx context.Context, assignment model.VerificationAssignment) bool {
	name := assignment.MachineName
	if name == "" {
		name = assignment.MachineID
	}
	fmt.Fprintf(v.out, "%s (%s)\n", assignment.JobID, name)

	destination, ok := v.targets[assignment.MachineID]
	if !ok {
		fmt.Fprintf(v.out, "  No evidence submitted: %s is not in %s. Add it and run again.\n",
			assignment.MachineID, v.targetsPath)
		return false
	}

	measurements := make([]verifierMeasurement, 0, len(verifierRules))
	complete := true
	for _, rule := range verifierRules {
		commandCtx, cancel := context.WithTimeout(ctx, verifierCommandTimeout)
		res := v.ssh(commandCtx, destination, rule.Command)
		cancel()
		judgement := rule.Judge(res)
		if !judgement.Observed {
			complete = false
		}
		measurements = append(measurements, verifierMeasurement{
			rule: rule, result: res, judgement: judgement, verifiedAt: v.now(),
		})
	}
	if complete {
		measurements, complete = v.settleGateway(ctx, destination, measurements)
	}

	evidence := make([]model.IndependentVerificationRequest, 0, len(verifierRules))
	for _, measurement := range measurements {
		if !measurement.judgement.Observed {
			fmt.Fprintf(v.out, "  %-26s unobserved  %s\n", measurement.rule.ID, unobservedReason(measurement.result))
			continue
		}
		fmt.Fprintf(v.out, "  %-26s %s  %s\n", measurement.rule.ID,
			passLabel(measurement.judgement.Passed), measurement.judgement.Summary)
		if measurement.settledFor > 0 {
			fmt.Fprintf(v.out, "  gateway settle window: adopting measurement after %s.\n", measurement.settledFor)
		}
		evidence = append(evidence, model.IndependentVerificationRequest{
			SchemaVersion: model.IndependentVerificationSchemaVersion,
			JobID:         assignment.JobID,
			RuleID:        measurement.rule.ID,
			Command:       excerpt(sshCommandLine(destination, measurement.rule.Command), maxVerifierCommandBytes),
			ExitCode:      measurement.result.ExitCode,
			StdoutExcerpt: excerpt(measurement.result.Stdout, maxExecOutput),
			StderrExcerpt: excerpt(measurement.result.Stderr, maxExecOutput),
			// ⚠ 留空是刻意的。裝好的 OpenClaw 是一棵展開的目錄，不是工作單那
			// 顆 artifact，對它取任何 hash 都不會等於工作單的 digest。
			// 填一個「看起來像 digest」的值會讓 Hub 判出假的 mismatch。
			ObservedDigest:  "",
			ObservedVersion: measurement.judgement.ObservedVersion,
			Passed:          measurement.judgement.Passed,
			VerifiedAt:      measurement.verifiedAt,
		})
	}
	if !complete {
		fmt.Fprintf(v.out, "  No evidence submitted: %d rules unobserved, assignment remains in pending report.\n", len(verifierRules))
		return false
	}
	if v.dryRun {
		fmt.Fprintf(v.out, "  --dry-run: observed %d rules, sent nothing.\n", len(evidence))
		return true
	}
	for _, row := range evidence {
		if err := postJSON(ctx, v.hubURL+"/v1/verifications", v.token, row, nil); err != nil {
			fmt.Fprintf(v.out, "  failed to submit %s: %v\n", row.RuleID, err)
			return false
		}
	}
	fmt.Fprintf(v.out, "  Submitted %d rows of evidence.\n", len(evidence))
	return true
}

// settleGateway avoids turning the measured service-start window into a
// durable failed verdict. It does not retry an inactive unit, an unobservable
// rule, or any report that is already incomplete.
func (v verifierRun) settleGateway(ctx context.Context, destination string,
	measurements []verifierMeasurement,
) ([]verifierMeasurement, bool) {
	gateway, unit := -1, -1
	for i := range measurements {
		switch measurements[i].rule.ID {
		case model.IndependentRuleOpenClawGatewayHTTP:
			gateway = i
		case model.IndependentRuleOpenClawUnitState:
			unit = i
		}
	}
	if gateway < 0 || unit < 0 || !measurements[gateway].judgement.Observed ||
		measurements[gateway].judgement.Passed || !measurements[unit].judgement.Passed {
		return measurements, true
	}
	wait := v.wait
	if wait == nil {
		wait = waitVerifierInterval
	}
	for elapsed := verifierGatewayRetryInterval; elapsed <= verifierGatewaySettleTimeout; elapsed += verifierGatewayRetryInterval {
		if !wait(ctx, verifierGatewayRetryInterval) {
			measurements[gateway].result = sshResult{Transport: true, Stderr: "gateway settle wait canceled"}
			measurements[gateway].judgement = verifierJudgement{}
			measurements[gateway].settledFor = elapsed - verifierGatewayRetryInterval
			return measurements, false
		}
		commandCtx, cancel := context.WithTimeout(ctx, verifierCommandTimeout)
		res := v.ssh(commandCtx, destination, measurements[gateway].rule.Command)
		cancel()
		judgement := measurements[gateway].rule.Judge(res)
		measurements[gateway].result = res
		measurements[gateway].judgement = judgement
		measurements[gateway].verifiedAt = v.now()
		measurements[gateway].settledFor = elapsed
		if !judgement.Observed {
			return measurements, false
		}
		if judgement.Passed {
			return measurements, true
		}
	}
	return measurements, true
}

func passLabel(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}

// unobservedReason 說的是「為什麼量不到」，講給要去修的人聽。
func unobservedReason(res sshResult) string {
	switch {
	case res.Transport:
		return "ssh cannot reach machine: " + excerpt(oneLine(res.Stderr), 200)
	case res.ExitCode == verifierToolMissingExit:
		return "machine is missing command required by this rule"
	case res.ExitCode != 0:
		return fmt.Sprintf("remote returned exit %d: %s", res.ExitCode, excerpt(oneLine(res.Stderr), 200))
	default:
		return "output is not expected key=value: " + excerpt(oneLine(res.Stdout), 200)
	}
}

func oneLine(text string) string {
	fields := strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) })
	return strings.Join(fields, " ")
}

// ---------------------------------------------------------------- targets

func loadVerifierTargets(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseVerifierTargets(raw)
}

func parseVerifierTargets(raw []byte) (map[string]string, error) {
	var file verifierTargetsFile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, err
	}
	if file.SchemaVersion != verifierTargetsSchema {
		return nil, fmt.Errorf("schema_version=%d, this version only recognizes %d",
			file.SchemaVersion, verifierTargetsSchema)
	}
	if len(file.Targets) == 0 {
		return nil, errors.New("targets is empty")
	}
	targets := make(map[string]string, len(file.Targets))
	for _, target := range file.Targets {
		if !validVerifierTargetValue(target.MachineID) {
			return nil, fmt.Errorf("invalid machine_id %q", target.MachineID)
		}
		// ⚠ 以 - 開頭的 destination 會被 ssh 當成選項讀走。那不是一台機器，
		// 那是一個讓別人替 ssh 加參數的洞。
		if !validVerifierTargetValue(target.SSHDestination) || strings.HasPrefix(target.SSHDestination, "-") {
			return nil, fmt.Errorf("invalid ssh_destination %q for %s", target.SSHDestination, target.MachineID)
		}
		if _, dup := targets[target.MachineID]; dup {
			return nil, fmt.Errorf("duplicate machine_id %s", target.MachineID)
		}
		targets[target.MachineID] = target.SSHDestination
	}
	return targets, nil
}

func validVerifierTargetValue(value string) bool {
	if value == "" || len(value) > 256 || value != strings.TrimSpace(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsSpace(char) || unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- ssh

// sshArgs 是這個 verifier 量測時真正用的 argv。證據裡記的那一行由同一個
// 函式產生，所以「記下來的指令」與「跑過的指令」不可能各走各的。
//
// BatchMode 讓它永遠不會停下來問密碼——一個會在 systemd 裡等輸入的 verifier
// 就是一個永遠不回報的 verifier。
func sshArgs(destination, command string) []string {
	return []string{"-n", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", destination, command}
}

// sshCommandLine 把同一組 argv 寫成一行可以直接貼回終端機重跑的指令。
// 一份沒辦法被第三個人重跑的證據，只是一個要人相信的說法。
func sshCommandLine(destination, command string) string {
	parts := []string{"ssh"}
	for _, arg := range sshArgs(destination, command) {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`;&|<>()*?[]#~=") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// runSSH 只做一件事：把一行 POSIX sh 交給遠端的 shell，把它說的話原樣帶回來。
func runSSH(ctx context.Context, destination, command string) sshResult {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(destination, command)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := sshResult{Stdout: stdout.String(), Stderr: stderr.String()}
	switch {
	case err == nil:
		return res
	case ctx.Err() != nil:
		res.Transport = true
		res.ExitCode = verifierSSHFailureExit
		if res.Stderr == "" {
			res.Stderr = "ssh timed out after " + verifierCommandTimeout.String() + " with no response"
		}
		return res
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			// ⚠ 負的 exit code 代表本地的 ssh 自己被訊號打死（OOM、
			// systemd 停 unit、有人 kill），遠端根本沒有回過任何狀態。
			// 把它當成規則失敗，等於用這一側的資源限制去判被測機器有罪。
			res.Transport = res.ExitCode == verifierSSHFailureExit || res.ExitCode < 0
			if res.ExitCode < 0 {
				res.ExitCode = verifierSSHFailureExit
				if res.Stderr == "" {
					res.Stderr = "ssh was terminated locally by a signal without receiving remote results"
				}
			}
			return res
		}
		// ssh 本身跑不起來（沒裝、PATH 不對）。那也是走不到遠端。
		res.Transport = true
		res.ExitCode = verifierSSHFailureExit
		res.Stderr = err.Error()
		return res
	}
}
