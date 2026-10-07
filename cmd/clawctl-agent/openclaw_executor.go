package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/probe"
	"github.com/teddashh/AI-Intune/internal/processenv"
	"github.com/teddashh/AI-Intune/internal/rollout"
)

const (
	openClawUnit = "openclaw-gateway.service"
	// ⚠ drop-in 不分目錄、一律按檔名字典序套用；機隊現有的 override.conf／proxy.conf
	// 都排在 "90-" 後面，哪天有人在裡面加 ExecStart 我們就會安靜地輸。"zz-" 保證最後。
	openClawDropIn           = "zz-clawctl-release.conf"
	maxExecOutput            = 4 << 10
	maxOpenClawArtifactBytes = int64(1 << 30)
)

type rejectError struct {
	Code   deploy.RejectionCode
	Detail string
}

func (e *rejectError) Error() string {
	return fmt.Sprintf("%s：%s", e.Code, e.Detail)
}

type execRunFunc func(context.Context, string, ...string) (stdout, stderr string, err error)
type execSystemctlFunc func(context.Context, ...string) (stdout, stderr string, err error)
type execHTTPGetFunc func(context.Context, string) (*http.Response, error)

// execDeps 集中所有會碰真機或外部 process 的入口；測試用 fsRoot 把絕對路徑
// 映到暫存根目錄，production 的空 fsRoot 則直接使用 discovery 量到的路徑。
type execDeps struct {
	home               string
	fsRoot             string
	systemctl          execSystemctlFunc
	run                execRunFunc
	antigravityVersion func(context.Context, string) (string, string, error)
	discover           func(context.Context) *model.OpenClawInstall
	dbDir              func() (dir string, layout string, ok bool)
	httpGet            execHTTPGetFunc
	hubURL             string
	token              string
	now                func() time.Time
	sleep              func(context.Context, time.Duration) error
}

type openclawExecutor struct {
	deps execDeps
}

type openClawRunSpec struct {
	Kind     string             `json:"kind"`
	Version  string             `json:"version"`
	Artifact *model.ArtifactRef `json:"artifact"`
}

type previousRelease struct {
	ExecStart     string `json:"exec_start"`
	RunningDir    string `json:"running_dir"`
	DropInExisted bool   `json:"drop_in_existed"`
	DropIn        []byte `json:"drop_in,omitempty"`
}

type switchSnapshot struct {
	dir             string
	previous        previousRelease
	dbDir           string
	dbSnapshot      string
	configSource    string
	configCopy      string
	openClawActive  bool
	openClawEnabled bool
	hermesActive    bool
	hermesEnabled   bool
	hermesImage     string
}

func defaultOpenClawExecutor(hubURL, token string) openclawExecutor {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	d := execDeps{home: home, hubURL: strings.TrimRight(hubURL, "/"), token: token, now: time.Now, sleep: sleepWithContext}
	d.run = runExecutorCommand
	d.systemctl = realSystemctl
	d.discover = func(ctx context.Context) *model.OpenClawInstall {
		return probe.DiscoverOpenClawInstall(ctx, home)
	}
	d.dbDir = func() (string, string, bool) { return probe.OpenClawDBDir(home) }
	d.httpGet = func(ctx context.Context, rawURL string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		// ⚠ token 只送回 Hub；把 agent bearer 帶到 localhost health 會把控制面
		// 憑證交給被升級的程式，這跟單純探活是兩回事。
		if d.token != "" && strings.HasPrefix(rawURL, d.hubURL+"/") {
			req.Header.Set("Authorization", "Bearer "+d.token)
		}
		return httpClient.Do(req)
	}
	return openclawExecutor{deps: d}
}

func runExecutorCommand(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := processenv.CommandContext(ctx, name, args...)
	stdout := limitBuffer{remaining: maxExecOutput}
	stderr := limitBuffer{remaining: maxExecOutput}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func realSystemctl(ctx context.Context, args ...string) (string, string, error) {
	return runExecutorCommand(ctx, "systemctl", args...)
}

func (e openclawExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := e.withDefaults()
	spec, install, port, err := d.gate(ctx, job)
	if err != nil {
		return nil, err
	}

	release := filepath.Join(d.releasesDir(), spec.Version)
	indexJS := filepath.Join(release, "lib", "node_modules", "openclaw", "dist", "index.js")
	runningReleaseDir := filepath.Join(release, "lib", "node_modules", "openclaw")
	if samePath(install.RunningDir, runningReleaseDir) && install.MainPID > 0 &&
		install.ProcessMatchesUnit != nil && *install.ProcessMatchesUnit {
		return d.verify(ctx, install.NodePath, indexJS, spec.Version, port, "already on target release"), nil
	}

	note, stageFailure, err := d.stage(ctx, job, spec, install, release)
	if err != nil {
		return nil, err
	}
	if stageFailure != nil {
		return []model.JobVerificationRequest{*stageFailure}, nil
	}

	snapshot, err := d.switchRelease(ctx, job, install, indexJS, release)
	if err != nil {
		failed := d.verification("switch", "switch OpenClaw release", "", err.Error(), 1, false)
		rollback := d.rollbackWithOwnBudget(ctx, job, install, snapshot, port)
		return []model.JobVerificationRequest{failed, rollback}, nil
	}

	verifications := d.verify(ctx, install.NodePath, indexJS, spec.Version, port, note)
	if allPassed(verifications) {
		return verifications, nil
	}
	verifications = append(verifications, d.rollbackWithOwnBudget(ctx, job, install, snapshot, port))
	return verifications, nil
}

// AfterSucceeded 清理的界線不是「留幾份」，而是這次成功之後還能用來退回、
// 或仍代表目前狀態的東西。每一條各自回報錯誤，不會倒過來改寫 Hub 已判定的成功。
func (e openclawExecutor) AfterSucceeded(_ context.Context, job model.JobResponse) []string {
	d := e.withDefaults()
	var reports []string
	reports = append(reports, d.retainSnapshots(job)...)
	reports = append(reports, d.retainFailedEvidence()...)
	reports = append(reports, d.retainReleases(job)...)
	return reports
}

func (d execDeps) retainSnapshots(job model.JobResponse) []string {
	dir := d.fsPath(d.snapshotsDir())
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []string{"did not clean snapshots: " + err.Error()}
	}
	keep := safeJobID(job.JobID)
	var reports []string
	for _, entry := range entries {
		if entry.Name() == keep {
			continue
		}
		// ⚠ 不是用數字留最近 N 份：每張單切換前都抄自己的快照，退回也只讀
		// 自己那份；本張成功後，更早快照對應的狀態已被這次成功取代。
		logical := filepath.Join(d.snapshotsDir(), entry.Name())
		if err := os.RemoveAll(d.fsPath(logical)); err != nil {
			reports = append(reports, "failed to clean snapshot "+logical+": "+err.Error())
			continue
		}
		reports = append(reports, "cleaned snapshot: "+logical)
	}
	return reports
}

func (d execDeps) retainFailedEvidence() []string {
	config := d.openClawConfigPath()
	root := filepath.Dir(config)
	prefixes := map[string]struct{}{filepath.Base(config) + ".failed-": {}}
	var reports []string
	if dbDir, _, ok := d.dbDir(); ok {
		if samePath(filepath.Dir(dbDir), root) {
			prefixes[filepath.Base(dbDir)+".failed-"] = struct{}{}
		} else {
			// ⚠ DB discovery 若意外回到 ~/.openclaw 外，不能跟著它擴張 RemoveAll
			// 的可達範圍；那會把專屬失敗證據清理變成任意目錄清理。
			reports = append(reports, "did not clean DB failure evidence: dbDir is not under ~/.openclaw: "+dbDir)
		}
	}
	entries, err := os.ReadDir(d.fsPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return reports
	}
	if err != nil {
		return append(reports, "did not clean failure evidence: "+err.Error())
	}
	for _, entry := range entries {
		matched := false
		for prefix := range prefixes {
			suffix, ok := strings.CutPrefix(entry.Name(), prefix)
			if ok && safeFailedEvidenceSuffix(suffix) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// ⚠ 不是按天數或份數清：這些 failed 檔都來自更早、曾停在中間的單；
		// 本機後來已成功，Hub 帳本裡的 verification 已取代它們作為失敗證據。
		logical := filepath.Join(root, entry.Name())
		if err := os.RemoveAll(d.fsPath(logical)); err != nil {
			reports = append(reports, "failed to clean failure evidence "+logical+": "+err.Error())
			continue
		}
		reports = append(reports, "cleaned failure evidence: "+logical)
	}
	return reports
}

func safeFailedEvidenceSuffix(suffix string) bool {
	if len(suffix) == 32 {
		if _, err := hex.DecodeString(suffix); err == nil {
			return true
		}
	}
	// ⚠ 只認 rollback 用 safeJobID 產生得出的 ASCII 形狀；若只看 `.failed-`
	// 就刪，使用者自己命名的備份也可能被當成 agent 證據清掉。
	return suffix != "" && safePathComponent(suffix) && safeJobID(suffix) == suffix
}

func (d execDeps) retainReleases(job model.JobResponse) []string {
	currentRoot, err := d.readCurrentRelease()
	if err != nil {
		// ⚠ current 不可信時整條跳過；猜錯目前目標再 RemoveAll，會把正在跑的
		// release 從磁碟刪掉。這比多留幾百 MB 嚴重得多。
		return []string{"did not clean releases: failed to read current: " + err.Error()}
	}
	keep := map[string]struct{}{filepath.Clean(currentRoot): {}}
	snapshot := filepath.Join(d.snapshotsDir(), safeJobID(job.JobID), "previous.json")
	b, readErr := os.ReadFile(d.fsPath(snapshot))
	if readErr == nil {
		var previous previousRelease
		if err := json.Unmarshal(b, &previous); err != nil {
			return []string{"did not clean releases: failed to read previous.json for this job: " + err.Error()}
		}
		if underDir(previous.RunningDir, d.releasesDir()) {
			keep[filepath.Clean(releaseRootOf(previous.RunningDir, d.releasesDir()))] = struct{}{}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return []string{"did not clean releases: failed to read previous.json for this job: " + readErr.Error()}
	}

	entries, err := os.ReadDir(d.fsPath(d.releasesDir()))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []string{"did not clean releases: " + err.Error()}
	}
	var reports []string
	for _, entry := range entries {
		logical := filepath.Join(d.releasesDir(), entry.Name())
		if _, kept := keep[filepath.Clean(logical)]; kept || strings.HasPrefix(entry.Name(), ".staging-") {
			continue
		}
		// ⚠ 不是固定留最近兩版：真正能退回的是本張 previous.json 的 running_dir，
		// 真正在跑的是 current；留任意兩個版本可能剛好刪掉其中一個。
		if err := os.RemoveAll(d.fsPath(logical)); err != nil {
			reports = append(reports, "failed to clean release "+logical+": "+err.Error())
			continue
		}
		reports = append(reports, "cleaned release: "+logical)
	}
	return reports
}

func (d execDeps) readCurrentRelease() (string, error) {
	current := filepath.Join(d.openClawRoot(), "current")
	target, err := os.Readlink(d.fsPath(current))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(current), target)
	}
	target = filepath.Clean(target)
	if !underDir(target, d.releasesDir()) {
		return "", fmt.Errorf("current points outside releases: %s", target)
	}
	root := releaseRootOf(target, d.releasesDir())
	info, err := os.Lstat(d.fsPath(root))
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("current target is not a release directory: %s", root)
	}
	return root, nil
}

// rollbackWithOwnBudget 讓退回不吃 executor 的 ctx。
//
// ⚠ health 等到 execution_timeout 用完才放棄時，那個 ctx 已經到期；exec.CommandContext
// 對到期的 ctx 會直接失敗，daemon-reload／stop／start 一個都跑不了，機器就停在
// 「新 drop-in ＋ 起不來的 gateway」。租約丟了（jobCtx 取消）也一樣要退。
// 退回自己再拿一次同樣的預算——Hub 開單時定的 execution_timeout——不另外發明秒數。
func (d execDeps) rollbackWithOwnBudget(parent context.Context, job model.JobResponse,
	install *model.OpenClawInstall, snapshot switchSnapshot, port int) model.JobVerificationRequest {
	ctx := context.WithoutCancel(parent)
	cancel := context.CancelFunc(func() {})
	if job.ExecutionTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(job.ExecutionTimeout)*time.Second)
	}
	defer cancel()
	return d.rollback(ctx, job, install, snapshot, port)
}

func (e openclawExecutor) withDefaults() execDeps {
	d := e.deps
	if d.now == nil {
		d.now = time.Now
	}
	if d.sleep == nil {
		d.sleep = sleepWithContext
	}
	if d.run == nil {
		d.run = runExecutorCommand
	}
	if d.systemctl == nil {
		d.systemctl = func(ctx context.Context, args ...string) (string, string, error) {
			return runExecutorCommand(ctx, "systemctl", args...)
		}
	}
	if d.discover == nil {
		d.discover = func(ctx context.Context) *model.OpenClawInstall {
			return probe.DiscoverOpenClawInstall(ctx, d.home)
		}
	}
	if d.dbDir == nil {
		d.dbDir = func() (string, string, bool) { return probe.OpenClawDBDir(d.home) }
	}
	if d.httpGet == nil {
		d.httpGet = func(ctx context.Context, rawURL string) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				return nil, err
			}
			if d.token != "" && strings.HasPrefix(rawURL, strings.TrimRight(d.hubURL, "/")+"/") {
				req.Header.Set("Authorization", "Bearer "+d.token)
			}
			return httpClient.Do(req)
		}
	}
	d.hubURL = strings.TrimRight(d.hubURL, "/")
	return d
}

func (d execDeps) gate(ctx context.Context, job model.JobResponse) (openClawRunSpec, *model.OpenClawInstall, int, error) {
	kind, artifact, err := model.ParseJobSpec(job.Spec)
	if err != nil {
		return openClawRunSpec{}, nil, 0, rejectPrecondition("job spec is not valid JSON: " + err.Error())
	}
	var spec openClawRunSpec
	if err := json.Unmarshal(job.Spec, &spec); err != nil {
		return spec, nil, 0, rejectPrecondition("job spec is not valid JSON: " + err.Error())
	}
	if kind != agentadapter.ExecutorKindOpenClaw || spec.Kind != agentadapter.ExecutorKindOpenClaw {
		return spec, nil, 0, rejectPrecondition(fmt.Sprintf("kind must be openclaw, got %q", kind))
	}
	if job.ResourceKind != agentadapter.ExecutorKindOpenClaw || job.ResourceID != agentadapter.ExecutorKindOpenClaw {
		return spec, nil, 0, rejectPrecondition("invalid OpenClaw job identity")
	}
	if artifact == nil || spec.Artifact == nil {
		return spec, nil, 0, rejectPrecondition("missing artifact")
	}
	if strings.TrimSpace(spec.Version) == "" {
		return spec, nil, 0, rejectPrecondition("version is empty")
	}
	if !safePathComponent(spec.Version) {
		return spec, nil, 0, rejectPrecondition("version is not a safe directory name: " + spec.Version)
	}
	if len(spec.Artifact.SHA256) != sha256.Size*2 {
		return spec, nil, 0, rejectPrecondition("artifact.sha256 is not 64 hex digits")
	}
	if _, err := hex.DecodeString(spec.Artifact.SHA256); err != nil {
		return spec, nil, 0, rejectPrecondition("artifact.sha256 is not hex: " + err.Error())
	}
	if spec.Artifact.URL == "" {
		return spec, nil, 0, rejectPrecondition("artifact.url is empty")
	}
	if !strings.HasPrefix(spec.Artifact.URL, "/") || strings.HasPrefix(spec.Artifact.URL, "//") {
		// ⚠ agent 只從自己的 Hub 抓：絕對 URL 會讓 agent 連去別的地方下載，
		// digest 擋得住位元組，擋不住那一次連線。
		return spec, nil, 0, rejectPrecondition("artifact.url must be relative to Hub (starting with /): " + spec.Artifact.URL)
	}
	if spec.Artifact.Size <= 0 || spec.Artifact.Size > maxOpenClawArtifactBytes {
		return spec, nil, 0, rejectPrecondition("artifact.size exceeds OpenClaw artifact limit")
	}
	if job.ArtifactDigest != "sha256:"+spec.Artifact.SHA256 {
		return spec, nil, 0, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: "job artifact digest does not match OpenClaw spec"}
	}

	install := d.discover(ctx)
	if install == nil {
		return spec, nil, 0, rejectPrecondition("discover did not return OpenClawInstall")
	}
	checks := []struct {
		ok     bool
		field  string
		reason string
	}{
		{install.UnitFound, "UnitFound", install.UnitReason},
		{install.RunningDir != "", "RunningDir", install.RunningDirReason},
		{install.NodePath != "", "NodePath", install.RunningDirReason},
		{install.NodeVersion != "", "NodeVersion", install.NodeVersionReason},
		{install.NpmPath != "", "NpmPath", install.NpmReason},
	}
	for _, check := range checks {
		if !check.ok {
			detail := "missing " + check.field
			if check.reason != "" {
				detail += ": " + check.reason
			}
			return spec, install, 0, rejectPrecondition(detail)
		}
	}
	resumableInactive := install.MainPID == 0 && (d.isManagedOpenClawInstall(install) || install.RunningDirExists)
	if !resumableInactive {
		if install.MainPID == 0 {
			return spec, install, 0, rejectPrecondition("missing MainPID: " + install.ProcessReason)
		}
		if install.ProcessMatchesUnit == nil {
			detail := "missing ProcessMatchesUnit"
			if install.ProcessReason != "" {
				detail += ": " + install.ProcessReason
			}
			return spec, install, 0, rejectPrecondition(detail)
		}
		if !*install.ProcessMatchesUnit {
			// ⚠ 擋的是 unit 已改、process 尚未重啟的未知狀態；在兩套座標上再疊
			// 一次升級，rollback 就不知道該回哪一套。
			return spec, install, 0, rejectPrecondition("ProcessMatchesUnit=false: unit was modified without restart; " + install.ProcessReason)
		}
	}
	port, ok := gatewayPort(install.GatewayArgs)
	if !ok {
		return spec, install, 0, rejectPrecondition("GatewayArgs missing --port N")
	}
	matched, understood := rollout.NodeSatisfiesRange(install.NodeVersion, spec.Artifact.EnginesNode)
	if !understood {
		return spec, install, 0, rejectPrecondition("unsupported engines syntax: " + spec.Artifact.EnginesNode)
	}
	if !matched {
		// ⚠ 擋的是 node 22.22.1/22.22.2 安裝 2026.7+ 後 npm 成功、舊 gateway
		// 已停，新的才因 engines >=22.22.3 起不來。
		return spec, install, 0, rejectPrecondition(fmt.Sprintf("NodeVersion %s does not satisfy engines %s", install.NodeVersion, spec.Artifact.EnginesNode))
	}

	share := filepath.Join(d.home, ".local", "share")
	if err := d.requireWritableAncestor(share); err != nil {
		return spec, install, 0, rejectPrecondition("~/.local/share is not writable: " + err.Error())
	}
	dropInDir := d.dropInDir()
	if err := d.requireWritableAncestor(dropInDir); err != nil {
		return spec, install, 0, rejectPrecondition("openclaw-gateway drop-in directory is not writable: " + err.Error())
	}
	return spec, install, port, nil
}

func (d execDeps) isManagedOpenClawInstall(install *model.OpenClawInstall) bool {
	root := d.openClawRoot()
	if install == nil || !samePath(install.UnitPath, filepath.Join(d.home, ".config", "systemd", "user", openClawUnit)) ||
		!samePath(install.NodePath, filepath.Join(d.home, ".local", "share", "clawctl", "node-runtime", "current", "bin", "node")) {
		return false
	}
	managedCurrent := filepath.Join(root, "current", "lib", "node_modules", "openclaw")
	return samePath(install.RunningDir, managedCurrent) || underDir(install.RunningDir, d.releasesDir())
}

func rejectPrecondition(detail string) error {
	return &rejectError{Code: deploy.PreconditionFailed, Detail: detail}
}

func (d execDeps) requireWritableAncestor(logical string) error {
	path := d.fsPath(logical)
	for {
		fi, err := os.Stat(path)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", logical)
			}
			return pathWritable(path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return err
		}
		path = parent
	}
}

func (d execDeps) stage(ctx context.Context, job model.JobResponse, spec openClawRunSpec,
	install *model.OpenClawInstall, release string) (string, *model.JobVerificationRequest, error) {
	if fi, err := os.Lstat(d.fsPath(release)); err == nil {
		if !fi.IsDir() {
			return "", d.stageFailed("existing release is not a directory"), nil
		}
		if _, _, validErr := d.validateRelease(ctx, release, spec.Version, install.NodePath); validErr == nil {
			return "reused existing release", nil, nil
		} else if ctx.Err() != nil {
			return "", d.stageFailed("context canceled while verifying existing release: " + ctx.Err().Error()), nil
		}
		broken := release + ".broken-" + safeJobID(job.JobID)
		if _, err := os.Lstat(d.fsPath(broken)); err == nil {
			return "", d.stageFailed("broken evidence path already exists, not overwriting: " + broken), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", d.stageFailed("cannot read broken evidence path: " + err.Error()), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return "", d.stageFailed("existing release verification failed and cannot be moved: " + err.Error()), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", d.stageFailed("cannot read release: " + err.Error()), nil
	}

	releases := d.releasesDir()
	staging := filepath.Join(releases, ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil { // 只清理這張工作單自己的 staging 殘骸。
		return "", d.stageFailed("failed to clean old staging: " + err.Error()), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return "", d.stageFailed("failed to create staging: " + err.Error()), nil
	}

	tgz := filepath.Join(staging, "openclaw.tgz")
	actualHash, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, tgz, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return "", nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: "OpenClaw artifact exceeds declared size"}
		}
		return "", d.stageFailed("failed to download artifact: " + err.Error()), nil
	}
	if !strings.EqualFold(actualHash, spec.Artifact.SHA256) || actualSize != spec.Artifact.Size {
		detail := fmt.Sprintf("artifact expected sha256=%s size=%d; got sha256=%s size=%d",
			shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualHash), actualSize)
		return "", nil, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: detail}
	}

	prefix := filepath.Join(staging, "prefix")
	scopeUnit := fmt.Sprintf("clawctl-stage-%s-%d", safeJobID(job.JobID), d.now().Unix())
	stdout, stderr, err := d.runInOwnScope(ctx, scopeUnit, install.NodePath, install.NpmPath,
		"install", "-g", "--prefix", prefix, tgz,
		"--no-audit", "--no-fund", "--loglevel=error")
	if err != nil {
		detail := fmt.Sprintf("npm install failed: %v", err)
		v := d.stageFailed(detail)
		v.StdoutExcerpt = excerpt(stdout, maxExecOutput)
		v.StderrExcerpt = excerpt(strings.TrimSpace(detail+"\n"+stderr), maxExecOutput)
		return "", v, nil
	}
	if fi, err := os.Lstat(d.fsPath(prefix)); err != nil || !fi.IsDir() {
		if err == nil {
			err = errors.New("npm-generated prefix is not a directory")
		}
		return "", d.stageFailed("invalid npm prefix layout: " + err.Error()), nil
	}
	stagedPackage := filepath.Join(prefix, "lib", "node_modules", "openclaw")
	stdout, stderr, err = d.validateRelease(ctx, prefix, spec.Version, install.NodePath)
	if err != nil {
		v := d.stageFailed(err.Error())
		v.Command = install.NodePath + " " + filepath.Join(stagedPackage, "dist", "index.js") + " --version"
		v.StdoutExcerpt = excerpt(stdout, maxExecOutput)
		v.StderrExcerpt = excerpt(strings.TrimSpace(err.Error()+"\n"+stderr), maxExecOutput)
		return "", v, nil
	}
	if err := os.Rename(d.fsPath(prefix), d.fsPath(release)); err != nil {
		return "", d.stageFailed("failed to publish staging release: " + err.Error()), nil
	}
	return "installed and verified new release", nil, nil
}

type artifactDownloadSizeError struct {
	Maximum int64
}

func (e *artifactDownloadSizeError) Error() string {
	return fmt.Sprintf("artifact exceeds maximum size %d", e.Maximum)
}

func (d execDeps) downloadArtifactAtMost(ctx context.Context, rawURL, logicalDest string, maximum int64) (string, int64, error) {
	resp, err := d.httpGet(ctx, rawURL)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxExecOutput))
		return "", 0, fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if maximum >= 0 && resp.ContentLength > maximum {
		return "", 0, &artifactDownloadSizeError{Maximum: maximum}
	}
	f, err := os.OpenFile(d.fsPath(logicalDest), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(d.fsPath(logicalDest))
		}
	}()
	h := sha256.New()
	reader := io.Reader(resp.Body)
	if maximum >= 0 {
		reader = io.LimitReader(resp.Body, maximum+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), reader)
	if err != nil {
		return "", n, err
	}
	if maximum >= 0 && n > maximum {
		return "", n, &artifactDownloadSizeError{Maximum: maximum}
	}
	if err := f.Sync(); err != nil {
		return "", n, err
	}
	if err := f.Close(); err != nil {
		return "", n, err
	}
	keep = true
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (d execDeps) validateRelease(ctx context.Context, prefix, version, nodePath string) (string, string, error) {
	pkgDir := filepath.Join(prefix, "lib", "node_modules", "openclaw")
	got, err := readPackageVersion(d.fsPath(filepath.Join(pkgDir, "package.json")))
	if err != nil {
		return "", "", fmt.Errorf("failed to read staged package.json: %w", err)
	}
	if got != version {
		return got, "", fmt.Errorf("staged package.json version=%q; want %q", got, version)
	}
	indexJS := filepath.Join(pkgDir, "dist", "index.js")
	stdout, stderr, err := d.run(ctx, nodePath, indexJS, "--version")
	if err != nil {
		return stdout, stderr, fmt.Errorf("staged OpenClaw --version failed: %w", err)
	}
	if !strings.Contains(stdout, version) {
		return stdout, stderr, fmt.Errorf("staged OpenClaw --version does not contain %q", version)
	}
	return stdout, stderr, nil
}

func readPackageVersion(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return "", err
	}
	if len(b) > 1<<20 {
		return "", errors.New("package.json exceeds 1 MiB")
	}
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	if p.Version == "" {
		return "", errors.New("version field is empty")
	}
	return p.Version, nil
}

// runInOwnScope 把一條指令搬進自己的 transient scope 跑，不留在 agent 的 cgroup 裡。
//
// ⚠ agent unit 有 MemoryMax=256M、CPUQuota=20%（ops/clawctl-agent.service）——那是給 agent 自己的。
// npm 在 agent 的 cgroup 裡跑會整套繼承：node 照 cgroup 上限把 V8 heap 縮到 252 MB，sampleagent4 2026-09-06
// npm 9 裝 2026.5.26 跑了 12 分鐘後 "Reached heap limit"（43c791e4）；samplehub1／sampleagent3 的 npm 11 只是剛好
// 擠得過（MemoryPeak 貼著 256M）。scope 裡的資源跟人手在那台打 npm install 一樣（samplehub1 實測：
// memory.max=max、heap 4 GB），時間仍由 execution_timeout 管。systemd-run --scope 是 exec 進去的，
// PID 不變，ctx 到期 kill 的就是 npm 本人；--collect 讓失敗的 scope 不留在 list-units。
func (d execDeps) runInOwnScope(ctx context.Context, unit, name string, args ...string) (string, string, error) {
	wrapped := append([]string{"--user", "--scope", "--quiet", "--collect", "--unit=" + unit, "--", name}, args...)
	return d.run(ctx, "systemd-run", wrapped...)
}

func (d execDeps) stageFailed(detail string) *model.JobVerificationRequest {
	v := d.verification("stage", "npm install and verify staging release", "", detail, 1, false)
	return &v
}

func (d execDeps) switchRelease(ctx context.Context, job model.JobResponse, install *model.OpenClawInstall,
	indexJS, release string) (switchSnapshot, error) {
	snapshot := switchSnapshot{
		dir: filepath.Join(d.snapshotsDir(), safeJobID(job.JobID)),
		previous: previousRelease{
			ExecStart:  install.ExecStart,
			RunningDir: install.RunningDir,
		},
		openClawActive:  unitIsActive(ctx, d, openClawUnit),
		openClawEnabled: unitIsEnabled(ctx, d, openClawUnit),
		hermesActive:    unitIsActive(ctx, d, hermesUnit),
		hermesEnabled:   unitIsEnabled(ctx, d, hermesUnit),
	}
	if snapshot.hermesActive {
		envPath := filepath.Join(d.home, ".config", "clawctl", "hermes.env")
		environment, readErr := readHermesEnvironment(d, envPath)
		if readErr != nil {
			return snapshot, fmt.Errorf("failed to read active Hermes identity: %w", readErr)
		}
		image, parseErr := hermesImageFromEnvironment(environment)
		if parseErr != nil {
			return snapshot, fmt.Errorf("invalid active Hermes identity: %w", parseErr)
		}
		snapshot.hermesImage = image
	}
	dropIn := filepath.Join(d.dropInDir(), openClawDropIn)
	if info, err := os.Lstat(d.fsPath(dropIn)); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 64<<10 {
			return snapshot, errors.New("invalid existing OpenClaw drop-in")
		}
		body, readErr := os.ReadFile(d.fsPath(dropIn))
		if readErr != nil {
			return snapshot, readErr
		}
		snapshot.previous.DropInExisted = true
		snapshot.previous.DropIn = body
	} else if !errors.Is(err, os.ErrNotExist) {
		return snapshot, err
	}
	if err := os.MkdirAll(d.fsPath(snapshot.dir), 0o700); err != nil {
		return snapshot, err
	}
	previousJSON, err := json.MarshalIndent(snapshot.previous, "", "  ")
	if err != nil {
		return snapshot, err
	}
	previousJSON = append(previousJSON, '\n')
	if err := atomicWriteFile(d.fsPath(filepath.Join(snapshot.dir, "previous.json")), previousJSON, 0o600); err != nil {
		return snapshot, err
	}
	content := dropInContent(install.NodePath, indexJS, install.GatewayArgs)
	if err := os.MkdirAll(d.fsPath(d.dropInDir()), 0o700); err != nil {
		return snapshot, err
	}
	if err := atomicWriteFile(d.fsPath(dropIn), []byte(content), 0o644); err != nil {
		return snapshot, err
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "daemon-reload"); err != nil {
		return snapshot, fmt.Errorf("systemctl daemon-reload failed: %v: %s", err, excerpt(stderr, maxExecOutput))
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "stop", openClawUnit); err != nil {
		return snapshot, fmt.Errorf("systemctl stop failed: %v: %s", err, excerpt(stderr, maxExecOutput))
	}
	if snapshot.hermesActive {
		if _, stderr, err := d.systemctl(ctx, "--user", "stop", hermesUnit); err != nil {
			return snapshot, fmt.Errorf("systemctl stop Hermes failed: %v: %s", err, excerpt(stderr, maxExecOutput))
		}
	}
	if snapshot.hermesEnabled {
		if _, stderr, err := d.systemctl(ctx, "--user", "disable", hermesUnit); err != nil {
			return snapshot, fmt.Errorf("systemctl disable Hermes failed: %v: %s", err, excerpt(stderr, maxExecOutput))
		}
	}

	// ⚠ 一定先 stop 才抄 DB；gateway 開著時直接複製 SQLite、WAL 與 SHM，
	// 三個檔可能來自不同瞬間，還原時就是一份半截資料庫。
	config := d.openClawConfigPath()
	snapshot.configSource = config
	if fi, err := os.Stat(d.fsPath(config)); err == nil && !fi.IsDir() {
		configCopy := filepath.Join(snapshot.dir, "openclaw.json")
		if err := copyFile(d.fsPath(config), d.fsPath(configCopy), 0o600); err != nil {
			return snapshot, fmt.Errorf("failed to snapshot openclaw.json: %w", err)
		}
		snapshot.configCopy = configCopy
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return snapshot, fmt.Errorf("failed to read openclaw.json: %w", err)
	}
	if dbDir, _, ok := d.dbDir(); ok {
		dbSnapshot := filepath.Join(snapshot.dir, filepath.Base(dbDir))
		if err := copyTree(d.fsPath(dbDir), d.fsPath(dbSnapshot)); err != nil {
			return snapshot, fmt.Errorf("failed to snapshot OpenClaw DB directory: %w", err)
		}
		snapshot.dbDir = dbDir
		snapshot.dbSnapshot = dbSnapshot
	}
	if err := d.configureLocalGateway(ctx, install.NodePath, indexJS); err != nil {
		return snapshot, err
	}
	if err := d.setCurrent(release); err != nil {
		return snapshot, fmt.Errorf("failed to switch current symlink: %w", err)
	}
	if !snapshot.openClawEnabled {
		if _, stderr, err := d.systemctl(ctx, "--user", "enable", openClawUnit); err != nil {
			return snapshot, fmt.Errorf("systemctl enable failed: %v: %s", err, excerpt(stderr, maxExecOutput))
		}
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "start", openClawUnit); err != nil {
		return snapshot, fmt.Errorf("systemctl start failed: %v: %s", err, excerpt(stderr, maxExecOutput))
	}
	return snapshot, nil
}

func (d execDeps) configureLocalGateway(ctx context.Context, nodePath, indexJS string) error {
	stdout, stderr, err := d.run(ctx, nodePath, indexJS, "config", "set", "gateway.mode", "local")
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(strings.Join([]string{stdout, stderr}, "\n"))
	if detail != "" {
		return fmt.Errorf("failed to set OpenClaw gateway.mode=local: %w: %s", err, excerpt(detail, maxExecOutput))
	}
	return fmt.Errorf("failed to set OpenClaw gateway.mode=local: %w", err)
}

func dropInContent(nodePath, indexJS string, args []string) string {
	command := strings.Join(append([]string{nodePath, indexJS}, args...), " ")
	return "[Service]\nExecStart=\nExecStart=" + command + "\n"
}

func (d execDeps) setCurrent(release string) error {
	current := filepath.Join(d.openClawRoot(), "current")
	tmp := current + ".tmp"
	_ = os.Remove(d.fsPath(tmp))
	if err := os.Symlink(filepath.Join("releases", filepath.Base(release)), d.fsPath(tmp)); err != nil {
		return err
	}
	if err := os.Rename(d.fsPath(tmp), d.fsPath(current)); err != nil {
		_ = os.Remove(d.fsPath(tmp))
		return err
	}
	return nil
}

// releaseRootOf 把 …/releases/<ver>/lib/node_modules/openclaw 收回 …/releases/<ver>；
// 呼叫端先用 underDir 確認 path 在 releasesDir 底下。
func releaseRootOf(path, releasesDir string) string {
	rel, err := filepath.Rel(filepath.Clean(releasesDir), filepath.Clean(path))
	if err != nil {
		return ""
	}
	return filepath.Join(releasesDir, strings.SplitN(rel, string(filepath.Separator), 2)[0])
}

func (d execDeps) verify(ctx context.Context, nodePath, indexJS, version string, port int, note string) []model.JobVerificationRequest {
	showOut, showErr, showRunErr := d.systemctl(ctx, "--user", "show", openClawUnit,
		"-p", "ActiveState", "-p", "MainPID", "-p", "ExecStart")
	props := parseSystemdProps(showOut)
	unitPassed := showRunErr == nil && props["ActiveState"] == "active" && strings.Contains(props["ExecStart"], indexJS)
	unitStdout := excerpt(showOut, maxExecOutput)
	if note != "" {
		unitStdout = note + "\n" + unitStdout
	}
	unit := d.verification("unit_execstart", "systemctl --user show "+openClawUnit+
		" -p ActiveState -p MainPID -p ExecStart", unitStdout, commandErr(showErr, showRunErr), exitCode(showRunErr), unitPassed)

	// 順序是規格：health 先、process_cmdline 後。理由見 processEvidence。
	health := d.health(ctx, port, "health")
	process := d.processEvidence(ctx, indexJS)
	stdout, stderr, versionErr := d.run(ctx, nodePath, indexJS, "--version")
	versionCheck := d.verification("version", nodePath+" "+indexJS+" --version", excerpt(stdout, maxExecOutput),
		commandErr(stderr, versionErr), exitCode(versionErr), versionErr == nil && strings.Contains(stdout, version))
	return []model.JobVerificationRequest{unit, health, process, versionCheck}
}

// processEvidence 讀 gateway 主行程真正的 argv，證明在跑的是新 release 的 dist/index.js。
//
// ⚠ 一定排在 health 之後，而且 "(…)" 形狀的 cmdline 要等、不是判失敗：systemd fork 出子行程後、
// execve 之前會先把它改名成 "(node)"（rename_process_from_path），這段時間 /proc/<MainPID>/cmdline
// 讀到的是佔位名，不是 gateway 的 argv。2026-09-06 金絲雀 f0537321 就是 start 一回來就讀，
// 讀到 "(node)"，把一個 health、version 都過的 release 退回了。
// health 過了代表 execve 早就發生，但 MainPID 可能已經不是 start 當下那個（Restart=always 下
// crash 一次就換 PID），所以每一輪都重問 systemd，不沿用 unit_execstart 那次的 PID。
// 讀到的是別的程式（例如舊的 index.js）就立刻失敗——那不是暫態，等再久也不會變成新 release。
func (d execDeps) processEvidence(ctx context.Context, indexJS string) model.JobVerificationRequest {
	const rule = "process_cmdline"
	for {
		showOut, showErr, showRunErr := d.systemctl(ctx, "--user", "show", openClawUnit, "-p", "MainPID")
		mainPID, _ := strconv.Atoi(parseSystemdProps(showOut)["MainPID"])
		procPath := filepath.Join("/proc", strconv.Itoa(mainPID), "cmdline")
		cmdline, procErr := os.ReadFile(d.fsPath(procPath))
		procText := strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
		switch {
		case showRunErr != nil:
			return d.verification(rule, procPath, "", "failed to query systemd MainPID: "+commandErr(showErr, showRunErr), exitCode(showRunErr), false)
		case mainPID > 0 && procErr == nil && cmdlineContains(cmdline, indexJS):
			return d.verification(rule, procPath, "MainPID="+strconv.Itoa(mainPID)+"\n"+procText, "", 0, true)
		case mainPID > 0 && procErr == nil && !systemdForkPlaceholder(procText):
			return d.verification(rule, procPath, procText,
				"MainPID "+strconv.Itoa(mainPID)+" is not running "+indexJS, 1, false)
		}
		// MainPID=0（unit 在重啟之間）、/proc 讀不到（PID 剛換）、或還是 "(node)" 佔位：等一下再問。
		if err := d.sleep(ctx, 2*time.Second); err != nil {
			last := procText
			if procErr != nil {
				last = procErr.Error()
			}
			return d.verification(rule, procPath, last,
				"timed out waiting for new release argv: MainPID="+strconv.Itoa(mainPID)+"; "+err.Error(), 1, false)
		}
	}
}

// systemdForkPlaceholder 認 systemd 在 execve 之前給子行程的名字："(node)" 這種頭尾括號的形狀，
// 或 cmdline 還是空的。只認形狀，不認 node 這個字：ExecStart 換成別的直譯器也一樣。
func systemdForkPlaceholder(cmdline string) bool {
	s := strings.TrimSpace(cmdline)
	return s == "" || (strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")"))
}

func (d execDeps) health(ctx context.Context, port int, rule string) model.JobVerificationRequest {
	rawURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	last := "no response received yet"
	for {
		resp, err := d.httpGet(ctx, rawURL)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxExecOutput+1))
			_ = resp.Body.Close()
			last = excerpt(string(body), maxExecOutput)
			var status struct {
				OK bool `json:"ok"`
			}
			jsonErr := json.Unmarshal(body, &status)
			if readErr == nil && resp.StatusCode == http.StatusOK && jsonErr == nil && status.OK {
				return d.verification(rule, "GET "+rawURL, last, "", 0, true)
			}
			if readErr != nil {
				last = readErr.Error()
			} else if jsonErr != nil {
				last = fmt.Sprintf("HTTP %d; JSON error: %v; body=%s", resp.StatusCode, jsonErr, last)
			} else {
				last = fmt.Sprintf("HTTP %d; body=%s", resp.StatusCode, last)
			}
		} else {
			last = err.Error()
		}
		if err := d.sleep(ctx, 2*time.Second); err != nil {
			return d.verification(rule, "GET "+rawURL, excerpt(last, maxExecOutput), err.Error(), 1, false)
		}
	}
}

func (d execDeps) rollback(ctx context.Context, job model.JobResponse, install *model.OpenClawInstall,
	snapshot switchSnapshot, port int) model.JobVerificationRequest {
	var problems []string
	dropIn := filepath.Join(d.dropInDir(), openClawDropIn)
	if snapshot.previous.DropInExisted {
		if err := atomicWriteFile(d.fsPath(dropIn), snapshot.previous.DropIn, 0o644); err != nil {
			problems = append(problems, "restore drop-in: "+err.Error())
		}
	} else if err := os.Remove(d.fsPath(dropIn)); err != nil && !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, "delete drop-in: "+err.Error())
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "daemon-reload"); err != nil {
		problems = append(problems, "daemon-reload: "+commandErr(stderr, err))
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "stop", openClawUnit); err != nil {
		problems = append(problems, "stop: "+commandErr(stderr, err))
	}

	failedSuffix := ".failed-" + safeJobID(job.JobID)
	currentDB := snapshot.dbDir
	if dir, _, ok := d.dbDir(); ok {
		currentDB = dir
	}
	if currentDB != "" {
		if _, err := os.Lstat(d.fsPath(currentDB)); err == nil {
			if err := renameWithoutOverwrite(d.fsPath(currentDB), d.fsPath(currentDB+failedSuffix)); err != nil {
				problems = append(problems, "preserve failed DB: "+err.Error())
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, "read failed DB: "+err.Error())
		}
	}
	if snapshot.dbSnapshot != "" {
		if err := copyTree(d.fsPath(snapshot.dbSnapshot), d.fsPath(snapshot.dbDir)); err != nil {
			problems = append(problems, "restore DB: "+err.Error())
		}
	}
	if snapshot.configSource != "" {
		if _, err := os.Lstat(d.fsPath(snapshot.configSource)); err == nil {
			if err := renameWithoutOverwrite(d.fsPath(snapshot.configSource), d.fsPath(snapshot.configSource+failedSuffix)); err != nil {
				problems = append(problems, "preserve failed openclaw.json: "+err.Error())
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, "read failed openclaw.json: "+err.Error())
		}
		if snapshot.configCopy != "" {
			if err := copyFile(d.fsPath(snapshot.configCopy), d.fsPath(snapshot.configSource), 0o600); err != nil {
				problems = append(problems, "restore openclaw.json: "+err.Error())
			}
		}
	}
	// ⚠ 擋的是退回之後 current 還指著失敗的 release：2026-09-06 f0537321 退回後
	// current -> releases/2026.6.6，gateway 跑的卻是 ~/.local/node_modules 那份；
	// 靠 current 回答「現在是哪一版」的人會被騙。previous 不在 releases 底下就沒有 current 可指。
	if underDir(snapshot.previous.RunningDir, d.releasesDir()) {
		if err := d.setCurrent(releaseRootOf(snapshot.previous.RunningDir, d.releasesDir())); err != nil {
			problems = append(problems, "restore current symlink: "+err.Error())
		}
	} else if err := os.Remove(d.fsPath(filepath.Join(d.openClawRoot(), "current"))); err != nil && !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, "remove current symlink: "+err.Error())
	}
	if !snapshot.openClawEnabled {
		if _, stderr, err := d.systemctl(ctx, "--user", "disable", openClawUnit); err != nil {
			problems = append(problems, "disable "+openClawUnit+": "+commandErr(stderr, err))
		}
	}
	if snapshot.hermesEnabled {
		if _, stderr, err := d.systemctl(ctx, "--user", "enable", hermesUnit); err != nil {
			problems = append(problems, "enable "+hermesUnit+": "+commandErr(stderr, err))
		}
	}
	if snapshot.openClawActive {
		if _, stderr, err := d.systemctl(ctx, "--user", "start", openClawUnit); err != nil {
			problems = append(problems, "start OpenClaw: "+commandErr(stderr, err))
		}
	}
	if snapshot.hermesActive {
		if _, stderr, err := d.systemctl(ctx, "--user", "start", hermesUnit); err != nil {
			problems = append(problems, "start Hermes: "+commandErr(stderr, err))
		}
	}
	var health model.JobVerificationRequest
	if snapshot.openClawActive {
		health = d.health(ctx, port, "rollback")
	} else {
		passed := true
		stdout := "previous runtime inactive"
		if snapshot.hermesActive {
			checks := (hermesExecutor{deps: d}).verify(ctx, d, model.HermesSpec{ImageReference: snapshot.hermesImage})
			passed = allPassed(checks)
			var evidence []string
			for _, check := range checks {
				evidence = append(evidence, check.RuleID+"="+strconv.FormatBool(check.Passed))
				if !check.Passed {
					problems = append(problems, "verify Hermes "+check.RuleID+": "+check.StderrExcerpt)
				}
			}
			stdout = strings.Join(evidence, " ")
		}
		health = d.verification("rollback", "restore previous agent runtime", stdout, "", 0, passed)
	}
	if len(problems) > 0 {
		health.Passed = false
		health.ExitCode = 1
		health.StderrExcerpt = excerpt(strings.Join(append(problems, health.StderrExcerpt), "; "), maxExecOutput)
	}
	if !health.Passed {
		// ⚠ 擋的是「rollback 指令跑過」被誤報成「舊 gateway 已回來」；
		// Restart=always 下 systemctl start 成功仍可能立即 crash-loop。
		health.StderrExcerpt = excerpt("health still failing after rollback; "+health.StderrExcerpt, maxExecOutput)
		log.Printf("ERROR: job %s health still failing after rollback: %s", job.JobID, health.StderrExcerpt)
	}
	_ = install // 保留呼叫形狀：rollback 的座標來自 previous.json，而非重新猜測。
	return health
}

func hermesImageFromEnvironment(environment string) (string, error) {
	lines := strings.Split(environment, "\n")
	if len(lines) != 3 || lines[2] != "" {
		return "", errors.New("invalid Hermes environment layout")
	}
	image, ok := strings.CutPrefix(lines[0], "HERMES_IMAGE=")
	if !ok || image == "" {
		return "", errors.New("missing HERMES_IMAGE")
	}
	version, ok := strings.CutPrefix(image, "docker.io/nousresearch/hermes-agent:v")
	if !ok || !validHermesExactVersion(version) {
		return "", errors.New("invalid HERMES_IMAGE")
	}
	index, ok := strings.CutPrefix(lines[1], "HERMES_IMAGE_INDEX=")
	if !ok || !validHermesDigest(index) {
		return "", errors.New("invalid HERMES_IMAGE_INDEX")
	}
	return image, nil
}

func (d execDeps) verification(rule, command, stdout, stderr string, code int, passed bool) model.JobVerificationRequest {
	return model.JobVerificationRequest{
		RuleID: rule, Command: command, ExitCode: code,
		StdoutExcerpt: excerpt(stdout, maxExecOutput), StderrExcerpt: excerpt(stderr, maxExecOutput),
		Passed: passed, VerifiedAt: d.now().UTC(),
	}
}

func allPassed(vs []model.JobVerificationRequest) bool {
	for _, v := range vs {
		if !v.Passed {
			return false
		}
	}
	return true
}

func gatewayPort(args []string) (int, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--port" {
			continue
		}
		port, err := strconv.Atoi(args[i+1])
		return port, err == nil && port > 0 && port <= 65535
	}
	return 0, false
}

func (d execDeps) fsPath(path string) string {
	if d.fsRoot == "" || path == "" {
		return path
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) {
		clean = strings.TrimPrefix(clean, string(filepath.Separator))
	}
	return filepath.Join(d.fsRoot, clean)
}

func (d execDeps) openClawRoot() string {
	return filepath.Join(d.home, ".local", "share", "clawctl", "openclaw")
}

func (d execDeps) openClawConfigPath() string {
	return filepath.Join(d.home, ".openclaw", "openclaw.json")
}

func (d execDeps) releasesDir() string  { return filepath.Join(d.openClawRoot(), "releases") }
func (d execDeps) snapshotsDir() string { return filepath.Join(d.openClawRoot(), "snapshots") }
func (d execDeps) dropInDir() string {
	return filepath.Join(d.home, ".config", "systemd", "user", openClawUnit+".d")
}

func atomicWriteFile(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".clawctl-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = out.Close()
		if !keep {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	keep = true
	return nil
}

func renameWithoutOverwrite(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("evidence path already exists, not overwriting: %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}

type limitBuffer struct {
	bytes.Buffer
	remaining int
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	if len(p) > 0 {
		_, _ = b.Buffer.Write(p)
		b.remaining -= len(p)
	}
	return original, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("snapshotting special file %s not supported (mode %s)", path, info.Mode())
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func parseSystemdProps(out string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	return props
}

func cmdlineContains(cmdline []byte, want string) bool {
	for _, arg := range bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0}) {
		if string(arg) == want {
			return true
		}
	}
	return false
}

func commandErr(stderr string, err error) string {
	parts := []string{}
	if err != nil {
		parts = append(parts, err.Error())
	}
	if strings.TrimSpace(stderr) != "" {
		parts = append(parts, strings.TrimSpace(stderr))
	}
	return strings.Join(parts, "：")
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func safePathComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value &&
		!strings.ContainsAny(value, `/\\`)
}

func safeJobID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 || b.String() == "." || b.String() == ".." {
		return "unknown"
	}
	return b.String()
}

func shortDigest(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func underDir(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// cleanOpenClawStaging 只刪 executor 自己命名的 .staging-*；正常 release、
// broken release、snapshot 與 ~/.openclaw 都不在這個函式的可達範圍。
func cleanOpenClawStaging(home, fsRoot string) ([]string, error) {
	cleaned, _, err := cleanOpenClawStagingKeeping(home, fsRoot, nil)
	return cleaned, err
}

// cleanOpenClawStagingKeeping 同上，但 keep 裡列的 staging 目錄名（.staging-<job>）留著不刪；
// 回傳（刪掉的邏輯路徑、留下的邏輯路徑、錯誤）。
// ⚠ 留下的是「scope 還沒停」的那幾張：刪掉它們 npm 會把 prefix 重新 mkdir 回來，等於白刪，
// 而且 openclaw.tgz 仍被佔著；下一次啟動 scope 停掉之後再清。
func cleanOpenClawStagingKeeping(home, fsRoot string, keep map[string]bool) (cleaned, kept []string, err error) {
	d := execDeps{home: home, fsRoot: fsRoot}
	dir := d.fsPath(d.releasesDir())
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".staging-") {
			continue
		}
		logical := filepath.Join(d.releasesDir(), entry.Name())
		if keep[entry.Name()] {
			kept = append(kept, logical)
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return cleaned, kept, err
		}
		cleaned = append(cleaned, logical)
	}
	return cleaned, kept, nil
}

// stageScopeStagingDir 把 stage 命名的 scope（clawctl-stage-<safeJobID>-<unix秒>.scope）
// 對回同一張單的 staging 目錄名（.staging-<safeJobID>）。unix 秒不含 '-'，所以切最後一個 '-'；
// 沒有 '-' 的名字（不是 stage 命的）整段當 job id，只會對到不存在的目錄，無害。
func stageScopeStagingDir(unit string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(unit, "clawctl-stage-"), ".scope")
	if i := strings.LastIndex(name, "-"); i > 0 {
		name = name[:i]
	}
	return ".staging-" + name
}

// parseStageScopeUnits 只接受 clawctl staging scope 的結構化 unit 名。
// ⚠ glob 是 systemd 端的第一層過濾；這裡仍驗前綴與副檔名，擋的是 pattern
// 打錯後誤把 openclaw-gateway.service 之類的 unit 交給 stop。
func parseStageScopeUnits(out string) []string {
	var units []string
	seen := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, "clawctl-stage-") || !strings.HasSuffix(name, ".scope") {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		units = append(units, name)
	}
	return units
}

func sweepOrphanStaging(ctx context.Context, home, fsRoot string, systemctl execSystemctlFunc) []string {
	stdout, _, listErr := systemctl(ctx, "--user", "list-units", "--type=scope", "--all", "--plain", "--no-legend", "clawctl-stage-*")
	units := parseStageScopeUnits(stdout)
	var reports []string
	if listErr != nil {
		reports = append(reports, "cannot list orphan scopes (not 0): "+listErr.Error())
	}

	// stillAlive：沒停掉（stop 失敗或預算用完沒輪到）的 scope 對應的 staging 目錄名；這些不刪。
	stillAlive := map[string]bool{}
	for i, unit := range units {
		if err := ctx.Err(); err != nil {
			reports = append(reports, fmt.Sprintf("orphan scope sweep aborted: %d orphan scopes remain unhandled: %v", len(units)-i, err))
			for _, left := range units[i:] {
				stillAlive[stageScopeStagingDir(left)] = true
			}
			break
		}
		// systemctl stop 會阻塞到 scope 停完；scope 預設以 control-group 為單位，
		// 先送 SIGTERM，逾時再由 systemd 送 SIGKILL。
		if _, _, err := systemctl(ctx, "--user", "stop", unit); err != nil {
			reports = append(reports, fmt.Sprintf("failed to stop orphan scope %s: %v", unit, err))
			stillAlive[stageScopeStagingDir(unit)] = true
			continue
		}
		reports = append(reports, "stopped orphan npm scope: "+unit)
	}

	// ⚠ 必須先停 scope 再刪 staging；反過來時 npm 會把 prefix 重新 mkdir 回去，
	// openclaw.tgz／prefix 裡的檔案也仍被佔著，下一張單的 stage 會撞到殘骸。
	// 所以沒停掉的 scope，它的 staging 也留著（stillAlive）。
	cleaned, kept, cleanErr := cleanOpenClawStagingKeeping(home, fsRoot, stillAlive)
	if cleanErr != nil {
		reports = append(reports, "failed to clean OpenClaw staging remnants (job loop still started): "+cleanErr.Error())
	}
	for _, path := range kept {
		reports = append(reports, "keeping "+path+": its npm scope has not stopped and would be recreated; will clean on next startup")
	}
	for _, path := range cleaned {
		reports = append(reports, "cleaned OpenClaw staging left from previous interruption: "+path)
	}
	if listErr == nil && len(units) == 0 && cleanErr == nil && len(cleaned) == 0 {
		reports = append(reports, "startup sweep: no orphan npm scopes, no staging remnants")
	}
	return reports
}
