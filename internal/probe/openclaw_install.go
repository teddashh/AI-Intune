package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const maxPackageJSON = 1 << 20

type installRunFunc func(context.Context, time.Duration, string, ...string) (string, string, error)
type installSystemctlFunc func(context.Context, time.Duration, ...string) (string, string, error)

// installDeps 把會碰到機器版面的入口集中起來。測試用 fsRoot 把 unit 裡的
// 絕對路徑映到暫存目錄，production 的空 fsRoot 則直接讀真實的唯讀路徑。
type installDeps struct {
	systemctl installSystemctlFunc
	run       installRunFunc
	home      string
	procRoot  string
	fsRoot    string
	agentPath string
}

func defaultInstallDeps(home string) installDeps {
	return installDeps{
		systemctl: func(ctx context.Context, timeout time.Duration, args ...string) (string, string, error) {
			return run(ctx, timeout, "systemctl", args...)
		},
		run:       run,
		home:      home,
		procRoot:  "/proc",
		agentPath: os.Getenv("PATH"),
	}
}

// DiscoverOpenClawInstall 用 production 的唯讀依賴量出 gateway 的實際安裝座標。
// 寫入 executor 只經由這個薄包裝取得事實，不另養一套容易漂移的 discovery。
func DiscoverOpenClawInstall(ctx context.Context, home string) *model.OpenClawInstall {
	return discoverInstall(ctx, defaultInstallDeps(home))
}

type parsedExecStart struct {
	raw        string
	nodePath   string
	indexJS    string
	runningDir string
	args       []string
}

// parseGatewayExecStart 只認 systemd show 的 argv[] 欄位，不嘗試重建 shell。
// systemd 已經把 ExecStart 正規化成 argv，這裡要保留的是它說的原文。
func parseGatewayExecStart(raw string) (parsedExecStart, error) {
	var candidates []string
	for rest := raw; ; {
		_, after, ok := strings.Cut(rest, "argv[]=")
		if !ok {
			break
		}
		argv, tail, ok := strings.Cut(after, " ; ignore_errors")
		if !ok {
			break
		}
		candidates = append(candidates, strings.TrimSpace(argv))
		rest = tail
	}
	if len(candidates) == 0 {
		if strings.TrimSpace(raw) == "" {
			return parsedExecStart{}, errors.New("ExecStart is empty")
		}
		return parsedExecStart{}, fmt.Errorf("ExecStart cannot find raw argv[]: %s", raw)
	}

	// 四台的 gateway unit 都是 Type=simple、一條 ExecStart（2026-09-06 實測）。
	// 真的印出多條時，這裡只能約定取最後一條；哪一條才是跑著的，交給下面
	// /proc/<MainPID>/cmdline 的比對去說，不在這裡假裝知道。
	argv := candidates[len(candidates)-1]
	p := parsedExecStart{raw: argv}
	fields := strings.Fields(argv)
	if len(fields) > 0 {
		p.nodePath = fields[0]
	}
	for i, arg := range fields {
		// ⚠ 只認 argv 裡真的以 dist/index.js 結尾的參數；不能假設 node 在
		// /usr/bin，也不能假設套件在 home 下，sampleagent3 與 sampleagent2 各自反證一半。
		if strings.HasSuffix(arg, "/dist/index.js") {
			p.indexJS = arg
			p.runningDir = strings.TrimSuffix(arg, "/dist/index.js")
			p.args = append([]string(nil), fields[i+1:]...)
			return p, nil
		}
	}
	return p, fmt.Errorf("ExecStart is not shaped like node .../dist/index.js: %s", argv)
}

// discoverInstall 只讀 systemd、套件目錄、/proc 與檔案系統統計。
// 任一格失敗只留下那一格的 Reason，不會抹掉其他已量到的事實。
func discoverInstall(ctx context.Context, deps installDeps) *model.OpenClawInstall {
	if deps.procRoot == "" {
		deps.procRoot = "/proc"
	}
	if deps.agentPath == "" {
		deps.agentPath = os.Getenv("PATH")
	}
	if deps.run == nil {
		deps.run = run
	}

	releasesDir := filepath.Join(deps.home, ".local", "share", "clawctl", "openclaw", "releases")
	i := &model.OpenClawInstall{ReleasesDir: releasesDir}
	// ⚠ release 版面與磁碟是 stat/statfs，跟 user bus 無關，所以排在 systemctl
	// 之前：systemctl 失敗就跳過它們，ReleasesPresent=false 沒有 Reason 可帶，
	// 「沒量」會長得跟「量到不存在」一模一樣。
	discoverReleaseLayout(i, deps)
	discoverDiskFree(i, deps)

	if deps.systemctl == nil {
		i.UnitReason = "systemctl runner 沒有設定"
		return i
	}
	// ⚠ 一次問完，避免查到一半 unit 剛好重載或重啟，拼出從沒同時存在過的
	// ExecStart、PID 與 NRestarts；也絕不查可能含密鑰的 Environment 欄位。
	out, stderr, err := deps.systemctl(ctx, cmdTimeout,
		"--user", "show", "openclaw-gateway.service",
		"-p", "FragmentPath", "-p", "DropInPaths", "-p", "ExecStart",
		"-p", "KillMode", "-p", "NRestarts", "-p", "ActiveEnterTimestamp",
		"-p", "MainPID")
	if err != nil {
		i.UnitReason = commandFailure("systemctl --user show openclaw-gateway.service", out, stderr, err)
		return i
	}
	props := parseProps(out)
	// ⚠ systemctl show 對不存在的 unit 仍可能 exit 0；用 exit code 會把
	// 「沒有 unit」說成「有 unit」，真正的分界是 FragmentPath 有沒有值。
	if props["FragmentPath"] == "" {
		i.UnitReason = "systemctl 回報 FragmentPath 為空：沒有 openclaw-gateway.service"
		return i
	}

	i.UnitFound = true
	i.UnitPath = props["FragmentPath"]
	if v := strings.TrimSpace(props["DropInPaths"]); v != "" && v != "(none)" {
		i.DropInPaths = strings.Fields(v)
	}
	i.KillMode = props["KillMode"]
	if n, err := strconv.Atoi(props["NRestarts"]); err == nil && props["NRestarts"] != "" {
		i.NRestarts = &n
	}
	i.ActiveEnterAt = parseSystemdTime(props["ActiveEnterTimestamp"])
	i.MainPID = atoiSafe(props["MainPID"])

	parsed, parseErr := parseGatewayExecStart(props["ExecStart"])
	i.ExecStart = parsed.raw
	i.NodePath = parsed.nodePath
	if parseErr != nil {
		i.RunningDirReason = parseErr.Error()
	} else {
		i.RunningDir = parsed.runningDir
		i.GatewayArgs = parsed.args
		discoverRunningDir(i, deps)
	}
	discoverProcess(i, deps, parsed.indexJS)
	discoverNode(ctx, i, deps)
	discoverNPM(ctx, i, deps)
	return i
}

func discoverRunningDir(i *model.OpenClawInstall, deps installDeps) {
	path := deps.fsPath(i.RunningDir)
	fi, err := os.Stat(path)
	if err != nil {
		i.RunningDirReason = appendReason(i.RunningDirReason, "讀不到執行目錄："+err.Error())
		return
	}
	if !fi.IsDir() {
		i.RunningDirReason = appendReason(i.RunningDirReason, "ExecStart 指的套件路徑不是目錄")
		return
	}
	i.RunningDirExists = true
	if owner, ok := unixFileOwner(fi); ok {
		i.RunningDirOwner = owner
	} else {
		i.RunningDirReason = appendReason(i.RunningDirReason, "stat 沒有 Unix uid")
	}

	if writable, reason := dirWritable(path); writable != nil {
		i.RunningDirWritable = writable
	} else if reason != "" {
		i.RunningDirReason = appendReason(i.RunningDirReason, reason)
	}

	version, err := readPackageVersion(filepath.Join(path, "package.json"))
	if err != nil {
		i.RunningDirReason = appendReason(i.RunningDirReason, "讀不到 package.json version："+err.Error())
		return
	}
	i.RunningDirVersion = version
}

func readPackageVersion(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// ⚠ 上限 1 MiB；package.json 若被換成巨檔或特殊來源，無界讀取會讓一筆
	// 本機觀測拖垮整個 agent，而不是只讓版本這一格失敗。
	b, err := io.ReadAll(io.LimitReader(f, maxPackageJSON+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxPackageJSON {
		return "", fmt.Errorf("package.json exceeds %d bytes limit", maxPackageJSON)
	}
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Version) == "" {
		return "", errors.New("version field is empty")
	}
	return strings.TrimSpace(p.Version), nil
}

func discoverProcess(i *model.OpenClawInstall, deps installDeps, unitIndexJS string) {
	if i.MainPID == 0 {
		i.ProcessReason = "unit 沒有 main process"
		return
	}
	path := filepath.Join(deps.procRoot, strconv.Itoa(i.MainPID), "cmdline")
	b, err := os.ReadFile(path)
	if err != nil {
		i.ProcessReason = "讀不到 /proc/<MainPID>/cmdline：" + err.Error()
		return
	}
	// ⚠ 只讀 cmdline；/proc/<pid>/exe 需要 ptrace 權限，agent 以非特權
	// User= 執行時四台都可能讀不到，拿它當入場券會製造「0 個 process」假象。
	for _, arg := range strings.Split(strings.TrimRight(string(b), "\x00"), "\x00") {
		if strings.HasSuffix(arg, "/dist/index.js") {
			i.ProcessIndexJS = arg
			if unitIndexJS == "" {
				i.ProcessReason = "ExecStart 裡沒有可供比對的 …/dist/index.js"
				return
			}
			v := unitIndexJS != "" && arg == unitIndexJS
			i.ProcessMatchesUnit = &v
			return
		}
	}
	i.ProcessReason = "cmdline 裡找不到 …/dist/index.js"
	if unitIndexJS != "" {
		v := false
		i.ProcessMatchesUnit = &v
	}
}

func discoverNode(ctx context.Context, i *model.OpenClawInstall, deps installDeps) {
	if i.NodePath == "" {
		return
	}
	// ⚠ 必須執行 ExecStart 的 argv[0] 絕對路徑；agent PATH 上的 node 在
	// sampleagent3 是另一個執行環境，量它會得到與 gateway 無關的答案。
	// Production deps.run is probe.run, so this --version shares the
	// process-lifetime version cache. A failed probe is not cached.
	out, stderr, err := deps.run(ctx, cmdTimeout, i.NodePath, "--version")
	if err != nil {
		i.NodeVersionReason = commandFailure(i.NodePath+" --version", out, stderr, err)
		return
	}
	i.NodeVersion = strings.TrimSpace(out)
	if i.NodeVersion == "" {
		i.NodeVersionReason = i.NodePath + " --version 沒有輸出"
	}
}

func discoverNPM(ctx context.Context, i *model.OpenClawInstall, deps installDeps) {
	if i.NodePath == "" {
		return
	}
	preferred := filepath.Join(filepath.Dir(i.NodePath), "npm")
	candidates := []string{}
	if isExecutable(deps.fsPath(preferred)) {
		candidates = append(candidates, preferred)
	}
	pathNPM := deps.lookPath("npm")
	if pathNPM != "" && pathNPM != preferred {
		candidates = append(candidates, pathNPM)
	}

	var failures []string
	for _, candidate := range candidates {
		// Same version cache as discoverNode when deps.run is probe.run.
		out, stderr, err := deps.run(ctx, cmdTimeout, candidate, "--version")
		if err != nil {
			failures = append(failures, commandFailure(candidate+" --version", out, stderr, err))
			continue
		}
		i.NpmPath = candidate
		i.NpmVersion = strings.TrimSpace(out)
		if i.NpmVersion == "" {
			failures = append(failures, candidate+" --version 沒有輸出")
			continue
		}
		return
	}
	// ⚠ npm 不一定在 /usr/bin：sampleagent3 跟 gateway 的 node 一起住在
	// ~/.local/node24/bin；只查 agent PATH 會錯過真正配套的 npm。
	prefix := fmt.Sprintf("找不到可用的 npm：試過 node 同目錄的 %s，也查過 agent PATH (%s)", preferred, deps.agentPath)
	if len(failures) > 0 {
		prefix += "；" + strings.Join(failures, "；")
	}
	i.NpmReason = prefix
}

func discoverReleaseLayout(i *model.OpenClawInstall, deps installDeps) {
	// ⚠ 這裡只 stat/readlink，絕不 mkdir 或補 symlink；第六刀（一）是在量
	// 升級版面是否存在，不是在機器上偷偷開始第六刀（三）。
	if fi, err := os.Stat(deps.fsPath(i.ReleasesDir)); err == nil && fi.IsDir() {
		i.ReleasesPresent = true
	}
	current := filepath.Join(deps.home, ".local", "share", "clawctl", "openclaw", "current")
	if target, err := os.Readlink(deps.fsPath(current)); err == nil {
		i.CurrentLink = target
	}
}

func discoverDiskFree(i *model.OpenClawInstall, deps installDeps) {
	target := filepath.Join(deps.home, ".local", "share")
	bytes, err := diskFreeAvailable(deps.fsPath(target))
	if err != nil {
		// ⚠ ~/.local/share 尚未存在不代表磁碟沒有空間；退到已存在的 home，
		// 但不為了讓 statfs 成功去建立目錄。
		target = deps.home
		bytes, err = diskFreeAvailable(deps.fsPath(target))
	}
	if err != nil {
		i.DiskFreeReason = diskFreeError(target, err)
		return
	}
	i.DiskFreeBytes = bytes
	i.DiskFreeMeasured = true
}

func (d installDeps) fsPath(path string) string {
	if d.fsRoot == "" || path == "" {
		return path
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) {
		clean = strings.TrimPrefix(clean, string(filepath.Separator))
	}
	return filepath.Join(d.fsRoot, clean)
}

func (d installDeps) lookPath(name string) string {
	for _, dir := range filepath.SplitList(d.agentPath) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if isExecutable(d.fsPath(candidate)) {
			return candidate
		}
	}
	return ""
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0
}

func appendReason(current, next string) string {
	if current == "" {
		return next
	}
	return current + "；" + next
}

func commandFailure(command, stdout, stderr string, err error) string {
	detail := strings.TrimSpace(strings.Join([]string{stdout, stderr}, " "))
	if detail == "" {
		return fmt.Sprintf("%s 失敗：%v", command, err)
	}
	return fmt.Sprintf("%s 失敗：%v（%s）", command, err, detail)
}
