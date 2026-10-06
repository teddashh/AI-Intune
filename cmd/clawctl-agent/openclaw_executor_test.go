package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	testHome    = "/home/tester"
	testVersion = "2026.6.10"
	testNode    = "/usr/bin/node"
	testNPM     = "/usr/bin/npm"
)

type execFixture struct {
	t           *testing.T
	root        string
	body        []byte
	install     *model.OpenClawInstall
	actions     []string
	mu          sync.Mutex
	npmCalls    int
	configCalls int
	healthOK    bool
	npmErr      error
	configErr   error
	stageVer    string
	startCalls  int
	mutateDB    bool
	scopeRuns   [][]string
	openActive  bool
	openEnable  bool
	hermActive  bool
	hermEnable  bool
	dbPresent   bool
}

func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	f := &execFixture{t: t, root: t.TempDir(), body: []byte("openclaw tarball"), healthOK: true,
		stageVer: testVersion, openActive: true, openEnable: true, dbPresent: true}
	matched := true
	f.install = &model.OpenClawInstall{
		UnitFound: true, ExecStart: testNode + " /opt/openclaw/dist/index.js gateway --port 18789",
		RunningDir: "/opt/openclaw", NodePath: testNode, NodeVersion: "v24.15.0",
		NpmPath: testNPM, MainPID: 1234, GatewayArgs: []string{"gateway", "--port", "18789"},
		ProcessMatchesUnit: &matched, ProcessIndexJS: "/opt/openclaw/dist/index.js",
	}
	for _, dir := range []string{
		filepath.Join(testHome, ".local", "share"),
		filepath.Join(testHome, ".config", "systemd", "user"),
		filepath.Join(testHome, ".openclaw", "state"),
		"/proc/4321",
	} {
		if err := os.MkdirAll(f.path(dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.write(filepath.Join(testHome, ".openclaw", "openclaw.json"), []byte(`{"token":"永遠不准印"}`), 0o600)
	f.write(filepath.Join(testHome, ".openclaw", "state", "openclaw.sqlite"), []byte("old-db"), 0o600)
	f.write(filepath.Join(testHome, ".openclaw", "state", "openclaw.sqlite-wal"), []byte("old-wal"), 0o600)
	f.write(filepath.Join(testHome, ".openclaw", "state", "openclaw.sqlite-shm"), []byte("old-shm"), 0o600)
	return f
}

func (f *execFixture) path(logical string) string {
	return filepath.Join(f.root, strings.TrimPrefix(filepath.Clean(logical), string(filepath.Separator)))
}

func (f *execFixture) write(logical string, b []byte, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(logical)), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.path(logical), b, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *execFixture) targetRelease() string {
	return filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "releases", testVersion)
}

func (f *execFixture) targetIndex() string {
	return filepath.Join(f.targetRelease(), "lib", "node_modules", "openclaw", "dist", "index.js")
}

func (f *execFixture) job() model.JobResponse {
	sum := sha256.Sum256(f.body)
	digest := hex.EncodeToString(sum[:])
	spec := fmt.Sprintf(`{"kind":"openclaw","version":%q,"artifact":{"sha256":%q,"size":%d,"url":"/v1/artifacts/%s","engines_node":">=22.22.3 <23 || >=24.15.0 <25 || >=25.9.0"}}`,
		testVersion, digest, len(f.body), digest)
	return model.JobResponse{
		JobID: "job-1", ResourceKind: agentadapter.ExecutorKindOpenClaw,
		ResourceID: agentadapter.ExecutorKindOpenClaw, Spec: []byte(spec), ArtifactDigest: "sha256:" + digest,
	}
}

func TestRunExecutorCommandDoesNotPassSystemdNotificationChannelsToChildren(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/run/user/1000/notify")
	t.Setenv("WATCHDOG_PID", "42")
	t.Setenv("WATCHDOG_USEC", "30000000")
	stdout, stderr, err := runExecutorCommand(context.Background(), "/bin/sh", "-c",
		`printf '%s|%s|%s' "${NOTIFY_SOCKET-}" "${WATCHDOG_PID-}" "${WATCHDOG_USEC-}"`)
	if err != nil {
		t.Fatalf("executor 子行程失敗：%v（stderr=%q）", err, stderr)
	}
	if stdout != "||" {
		t.Fatalf("systemd notification 控制變數流進 executor 子行程：%q", stdout)
	}
}

func (f *execFixture) deps() execDeps {
	return execDeps{
		home: testHome, fsRoot: f.root, hubURL: "https://hub.example", token: "secret", now: func() time.Time { return jobsTestNow }, sleep: sleepWithContext,
		discover: func(context.Context) *model.OpenClawInstall { copy := *f.install; return &copy },
		dbDir: func() (string, string, bool) {
			if !f.dbPresent {
				return "", "", false
			}
			return filepath.Join(testHome, ".openclaw", "state"), "consolidated", true
		},
		httpGet: func(ctx context.Context, rawURL string) (*http.Response, error) {
			if strings.Contains(rawURL, "/v1/artifacts/") {
				return response(http.StatusOK, f.body), nil
			}
			if f.healthOK {
				return response(http.StatusOK, []byte(`{"ok":true,"status":"live"}`)), nil
			}
			return response(http.StatusServiceUnavailable, []byte(`{"ok":false}`)), nil
		},
		run: func(ctx context.Context, name string, args ...string) (string, string, error) {
			if name == "systemd-run" {
				raw := append([]string{name}, args...)
				name, args = unwrapScope(name, args)
				if name == hermesPodmanPath {
					if len(args) >= 2 && args[0] == "container" && args[1] == "inspect" && f.hermActive {
						return "true|docker.io/nousresearch/hermes-agent:v2026.9.7\n", "", nil
					}
					if len(args) >= 3 && args[0] == "image" && args[1] == "exists" {
						return "", "", nil
					}
					return "", "", errors.New("Hermes container unavailable")
				}
				f.scopeRuns = append(f.scopeRuns, raw)
			}
			if name == f.install.NodePath && len(args) > 0 && args[0] == f.install.NpmPath {
				args = args[1:]
				f.npmCalls++
				if f.npmErr != nil {
					return "", "npm exploded", f.npmErr
				}
				prefix := args[3]
				pkg := filepath.Join(prefix, "lib", "node_modules", "openclaw")
				f.write(filepath.Join(pkg, "package.json"), []byte(fmt.Sprintf(`{"version":%q}`, f.stageVer)), 0o600)
				f.write(filepath.Join(pkg, "dist", "index.js"), []byte("index"), 0o600)
				return "installed", "", nil
			}
			if name == f.install.NodePath && len(args) == 5 && args[0] == f.targetIndex() &&
				reflect.DeepEqual(args[1:], []string{"config", "set", "gateway.mode", "local"}) {
				f.configCalls++
				if f.configErr != nil {
					return "", "config write failed", f.configErr
				}
				config := filepath.Join(testHome, ".openclaw", "openclaw.json")
				if _, err := os.Stat(f.path(config)); errors.Is(err, os.ErrNotExist) {
					f.write(config, []byte("{\n  \"gateway\": {\n    \"mode\": \"local\"\n  }\n}\n"), 0o600)
				}
				return "Updated gateway.mode. Restart the gateway to apply.\n", "", nil
			}
			if name == f.install.NodePath {
				select {
				case <-ctx.Done():
					return "", "", ctx.Err()
				default:
				}
				return "OpenClaw " + testVersion + " (deadbeef)\n", "", nil
			}
			return "", "", fmt.Errorf("unexpected run: %s %v", name, args)
		},
		systemctl: func(ctx context.Context, args ...string) (string, string, error) {
			if len(args) >= 3 && args[1] == "is-active" {
				active := f.openActive
				if args[len(args)-1] == hermesUnit {
					active = f.hermActive
				}
				if active {
					return "active\n", "", nil
				}
				return "inactive\n", "", errors.New("exit 3")
			}
			if len(args) >= 3 && args[1] == "is-enabled" {
				enabled := f.openEnable
				if args[len(args)-1] == hermesUnit {
					enabled = f.hermEnable
				}
				if enabled {
					return "enabled\n", "", nil
				}
				return "disabled\n", "", errors.New("exit 1")
			}
			if len(args) >= 2 && args[1] == "show" {
				return "ActiveState=active\nMainPID=4321\nExecStart={ path=" + f.install.NodePath + " ; argv[]=" + f.install.NodePath + " " + f.targetIndex() + " gateway --port 18789 ; ignore_errors=no ; }\n", "", nil
			}
			action := strings.Join(args[1:], " ")
			f.mu.Lock()
			f.actions = append(f.actions, action)
			if len(args) >= 3 {
				switch args[1] {
				case "start", "restart":
					if args[2] == openClawUnit {
						f.openActive = true
					} else if args[2] == hermesUnit {
						f.hermActive = true
					}
				case "stop":
					if args[2] == openClawUnit {
						f.openActive = false
					} else if args[2] == hermesUnit {
						f.hermActive = false
					}
				case "enable", "disable":
					enabled := args[1] == "enable"
					if args[2] == openClawUnit {
						f.openEnable = enabled
					} else if args[2] == hermesUnit {
						f.hermEnable = enabled
					}
				}
			}
			if len(args) >= 2 && args[1] == "start" {
				f.startCalls++
				if f.mutateDB && f.startCalls == 1 {
					f.write(filepath.Join(testHome, ".openclaw", "state", "openclaw.sqlite"), []byte("new-db"), 0o600)
					f.write(filepath.Join(testHome, ".openclaw", "openclaw.json"), []byte(`{"token":"new"}`), 0o600)
					f.dbPresent = true
				}
			}
			f.mu.Unlock()
			return "", "", nil
		},
	}
}

func response(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}
}

func (f *execFixture) prepareProc() {
	f.write("/proc/4321/cmdline", append([]byte(f.install.NodePath+"\x00"+f.targetIndex()+"\x00gateway\x00--port\x0018789"), 0), 0o600)
}

func (f *execFixture) prepareHermesIdentity() {
	f.write(filepath.Join(testHome, ".config", "clawctl", "hermes.env"),
		[]byte("HERMES_IMAGE=docker.io/nousresearch/hermes-agent:v2026.9.7\nHERMES_IMAGE_INDEX=sha256:"+
			strings.Repeat("a", 64)+"\n"), 0o600)
}

func fsFingerprint(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		got = append(got, fmt.Sprintf("%s|%s|%d|%d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

func TestOpenClawGateRejectsWithoutWritingOrSystemctl(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*execFixture, *model.JobResponse)
		part   string
	}{
		{"bad json", func(_ *execFixture, j *model.JobResponse) {
			j.Spec = []byte(`{"kind":`)
		}, "spec 不是合法 JSON"},
		{"kind", func(_ *execFixture, j *model.JobResponse) {
			j.Spec = []byte(`{"kind":"other","version":"x","artifact":{}}`)
		}, "kind 必須"},
		{"resource kind", func(_ *execFixture, j *model.JobResponse) { j.ResourceKind = "other" }, "job identity"},
		{"resource id", func(_ *execFixture, j *model.JobResponse) { j.ResourceID = "other" }, "job identity"},
		{"artifact", func(_ *execFixture, j *model.JobResponse) { j.Spec = []byte(`{"kind":"openclaw","version":"x"}`) }, "artifact 缺少"},
		{"version", func(_ *execFixture, j *model.JobResponse) { j.Spec = []byte(`{"kind":"openclaw","artifact":{}}`) }, "version 是空的"},
		{"artifact size", func(_ *execFixture, j *model.JobResponse) {
			j.Spec = []byte(strings.Replace(string(j.Spec), fmt.Sprintf(`"size":%d`, len("openclaw tarball")), `"size":0`, 1))
		}, "artifact.size"},
		{"unit", func(f *execFixture, _ *model.JobResponse) {
			f.install.UnitFound = false
			f.install.UnitReason = "unit reason"
		}, "UnitFound 缺少：unit reason"},
		{"running dir", func(f *execFixture, _ *model.JobResponse) {
			f.install.RunningDir = ""
			f.install.RunningDirReason = "dir reason"
		}, "RunningDir 缺少：dir reason"},
		{"node path", func(f *execFixture, _ *model.JobResponse) {
			f.install.NodePath = ""
			f.install.RunningDirReason = "node reason"
		}, "NodePath 缺少：node reason"},
		{"node version", func(f *execFixture, _ *model.JobResponse) {
			f.install.NodeVersion = ""
			f.install.NodeVersionReason = "version reason"
		}, "NodeVersion 缺少：version reason"},
		{"npm", func(f *execFixture, _ *model.JobResponse) { f.install.NpmPath = ""; f.install.NpmReason = "npm reason" }, "NpmPath 缺少：npm reason"},
		{"pid", func(f *execFixture, _ *model.JobResponse) {
			f.install.MainPID = 0
			f.install.ProcessReason = "pid reason"
		}, "MainPID 缺少：pid reason"},
		{"process unknown", func(f *execFixture, _ *model.JobResponse) {
			f.install.ProcessMatchesUnit = nil
			f.install.ProcessReason = "proc reason"
		}, "ProcessMatchesUnit 缺少：proc reason"},
		{"process mismatch", func(f *execFixture, _ *model.JobResponse) { no := false; f.install.ProcessMatchesUnit = &no }, "unit 改過但沒重啟"},
		{"port", func(f *execFixture, _ *model.JobResponse) { f.install.GatewayArgs = []string{"gateway"} }, "GatewayArgs 缺少 --port N"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecFixture(t)
			job := f.job()
			tc.mutate(f, &job)
			before := fsFingerprint(t, f.root)
			deps := f.deps()
			calls := 0
			deps.systemctl = func(context.Context, ...string) (string, string, error) { calls++; return "", "", nil }
			_, err := (openclawExecutor{deps: deps}).Run(context.Background(), job)
			var rejected *rejectError
			if !errors.As(err, &rejected) || rejected.Code != deploy.PreconditionFailed || !strings.Contains(rejected.Detail, tc.part) {
				t.Fatalf("error=%#v；要 PRECONDITION_FAILED 且含 %q", err, tc.part)
			}
			if after := fsFingerprint(t, f.root); !reflect.DeepEqual(before, after) {
				t.Errorf("閘門改了檔案系統\nbefore=%v\nafter=%v", before, after)
			}
			if calls != 0 {
				t.Errorf("閘門失敗仍呼叫 systemctl %d 次", calls)
			}
		})
	}
}

func TestOpenClawGateAcceptsStoppedManagedReleaseAfterHermesWasSelected(t *testing.T) {
	f := newExecFixture(t)
	f.install.UnitPath = filepath.Join(testHome, ".config", "systemd", "user", openClawUnit)
	f.install.NodePath = filepath.Join(testHome, ".local", "share", "clawctl", "node-runtime", "current", "bin", "node")
	f.install.NpmPath = filepath.Join(filepath.Dir(f.install.NodePath), "npm")
	f.install.RunningDir = filepath.Join(f.targetRelease(), "lib", "node_modules", "openclaw")
	f.install.MainPID = 0
	f.install.ProcessMatchesUnit = nil
	f.install.ProcessReason = "unit 沒有 main process"
	if _, _, port, err := f.deps().gate(context.Background(), f.job()); err != nil || port != 18789 {
		t.Fatalf("stopped managed release gate port=%d err=%v", port, err)
	}
}

func TestOpenClawGateAcceptsStoppedExistingInstallAfterHermesWasSelected(t *testing.T) {
	f := newExecFixture(t)
	f.install.RunningDirExists = true
	f.install.MainPID = 0
	f.install.ProcessMatchesUnit = nil
	f.install.ProcessReason = "unit 沒有 main process"
	if _, _, port, err := f.deps().gate(context.Background(), f.job()); err != nil || port != 18789 {
		t.Fatalf("stopped existing install gate port=%d err=%v", port, err)
	}
}

func TestOpenClawGateRejectsUnknownEnginesAndUnwritableRootsWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*execFixture, *model.JobResponse)
		part   string
	}{
		{
			"unknown engines",
			func(f *execFixture, job *model.JobResponse) {
				old := ">=22.22.3 <23 || >=24.15.0 <25 || >=25.9.0"
				job.Spec = []byte(strings.Replace(string(job.Spec), old, "^24.0.0", 1))
			},
			"engines 語法我不會判：^24.0.0",
		},
		{
			"share unwritable",
			func(f *execFixture, _ *model.JobResponse) {
				if err := os.Chmod(f.path(filepath.Join(testHome, ".local", "share")), 0o500); err != nil {
					t.Fatal(err)
				}
			},
			"~/.local/share 不可寫",
		},
		{
			"dropin unwritable",
			func(f *execFixture, _ *model.JobResponse) {
				if err := os.Chmod(f.path(filepath.Join(testHome, ".config", "systemd", "user")), 0o500); err != nil {
					t.Fatal(err)
				}
			},
			"drop-in 目錄不可寫",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecFixture(t)
			job := f.job()
			tc.mutate(f, &job)
			before := fsFingerprint(t, f.root)
			deps := f.deps()
			calls := 0
			deps.systemctl = func(context.Context, ...string) (string, string, error) { calls++; return "", "", nil }
			_, err := (openclawExecutor{deps: deps}).Run(context.Background(), job)
			var rejected *rejectError
			if !errors.As(err, &rejected) || rejected.Code != deploy.PreconditionFailed || !strings.Contains(rejected.Detail, tc.part) {
				t.Fatalf("error=%#v；要拒單且含 %q", err, tc.part)
			}
			if after := fsFingerprint(t, f.root); !reflect.DeepEqual(before, after) {
				t.Errorf("閘門寫了檔案：before=%v after=%v", before, after)
			}
			if calls != 0 {
				t.Errorf("閘門失敗仍呼叫 systemctl %d 次", calls)
			}
		})
	}
}

func TestArtifactHashMismatchRejectsAndCleansStaging(t *testing.T) {
	f := newExecFixture(t)
	job := f.job()
	f.body = []byte("tampered")
	deps := f.deps()
	_, err := (openclawExecutor{deps: deps}).Run(context.Background(), job)
	var rejected *rejectError
	if !errors.As(err, &rejected) || rejected.Code != deploy.ArtifactHashMismatch {
		t.Fatalf("error=%#v；要 ARTIFACT_HASH_MISMATCH", err)
	}
	if _, err := os.Stat(f.path(filepath.Join(filepath.Dir(f.targetRelease()), ".staging-job-1"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging 沒清掉：%v", err)
	}
	if len(f.actions) != 0 {
		t.Errorf("hash mismatch 仍碰 systemctl：%v", f.actions)
	}
}

func TestOpenClawJobDigestMismatchRejectsBeforeMutation(t *testing.T) {
	f := newExecFixture(t)
	job := f.job()
	job.ArtifactDigest = "sha256:" + strings.Repeat("f", 64)
	before := fsFingerprint(t, f.root)
	_, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), job)
	var rejected *rejectError
	if !errors.As(err, &rejected) || rejected.Code != deploy.ArtifactHashMismatch ||
		!reflect.DeepEqual(before, fsFingerprint(t, f.root)) {
		t.Fatalf("digest mismatch error=%v", err)
	}
}

func TestOpenClawDownloadStopsAtDeclaredArtifactSize(t *testing.T) {
	f := newExecFixture(t)
	job := f.job()
	f.body = append(f.body, []byte("extra bytes")...)
	_, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), job)
	var rejected *rejectError
	if !errors.As(err, &rejected) || rejected.Code != deploy.ArtifactHashMismatch ||
		!strings.Contains(rejected.Detail, "超過宣告 size") {
		t.Fatalf("oversized response error=%v", err)
	}
}

func TestStageFailuresDoNotStopGatewayAndCleanStaging(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*execFixture)
	}{
		{"npm", func(f *execFixture) { f.npmErr = errors.New("exit 1") }},
		{"version", func(f *execFixture) { f.stageVer = "2026.6.9" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecFixture(t)
			tc.mutate(f)
			vs, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), f.job())
			if err != nil || len(vs) != 1 || vs[0].RuleID != "stage" || vs[0].Passed {
				t.Fatalf("verifications=%+v err=%v", vs, err)
			}
			if strings.Contains(strings.Join(f.actions, ","), "stop") {
				t.Errorf("stage 失敗仍 stop：%v", f.actions)
			}
			if _, err := os.Stat(f.path(filepath.Join(filepath.Dir(f.targetRelease()), ".staging-job-1"))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("staging 沒清掉：%v", err)
			}
		})
	}
}

func TestOpenClawHappyPath(t *testing.T) {
	f := newExecFixture(t)
	f.prepareProc()
	vs, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), f.job())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.actions, []string{"daemon-reload", "stop openclaw-gateway.service", "start openclaw-gateway.service"}; !reflect.DeepEqual(got, want) {
		t.Errorf("systemctl=%v；要 %v", got, want)
	}
	dropIn := filepath.Join(testHome, ".config", "systemd", "user", openClawUnit+".d", openClawDropIn)
	wantDropIn := dropInContent(testNode, f.targetIndex(), f.install.GatewayArgs)
	if got, err := os.ReadFile(f.path(dropIn)); err != nil || string(got) != wantDropIn {
		t.Errorf("drop-in=%q err=%v；要 %q", got, err, wantDropIn)
	}
	snapshot := filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "snapshots", "job-1")
	if info, err := os.Stat(f.path(filepath.Join(snapshot, "openclaw.json"))); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("config snapshot mode=%v err=%v", info, err)
	}
	for _, name := range []string{"openclaw.sqlite", "openclaw.sqlite-wal", "openclaw.sqlite-shm"} {
		if _, err := os.Stat(f.path(filepath.Join(snapshot, "state", name))); err != nil {
			t.Errorf("snapshot 缺 %s：%v", name, err)
		}
	}
	current := filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "current")
	if got, err := os.Readlink(f.path(current)); err != nil || got != filepath.Join("releases", testVersion) {
		t.Errorf("current=%q err=%v", got, err)
	}
	if len(vs) != 4 || !allPassed(vs) {
		t.Errorf("四條驗證應全過：%+v", vs)
	}
	var rules []string
	for _, verification := range vs {
		rules = append(rules, verification.RuleID)
	}
	if want := []string{"unit_execstart", "health", "process_cmdline", "version"}; !reflect.DeepEqual(rules, want) {
		t.Errorf("驗證規則=%v；要 %v", rules, want)
	}
	if _, err := os.Stat(f.path(filepath.Join(filepath.Dir(f.targetRelease()), ".staging-job-1"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("快樂路徑留下 staging：%v", err)
	}
}

func TestOpenClawFreshMachineActivatesManagedUnitAndReplacesHermes(t *testing.T) {
	f := newExecFixture(t)
	managedNode := filepath.Join(testHome, ".local", "share", "clawctl", "node-runtime", "current", "bin", "node")
	f.install.UnitPath = filepath.Join(testHome, ".config", "systemd", "user", openClawUnit)
	f.install.NodePath = managedNode
	f.install.NpmPath = filepath.Join(filepath.Dir(managedNode), "npm")
	f.install.RunningDir = filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "current",
		"lib", "node_modules", "openclaw")
	f.install.ExecStart = managedNode + " " + filepath.Join(f.install.RunningDir, "dist", "index.js") +
		" gateway --port 18789"
	f.install.MainPID = 0
	f.install.ProcessMatchesUnit = nil
	f.install.ProcessIndexJS = ""
	f.install.ProcessReason = "unit 沒有 main process"
	f.openActive = false
	f.openEnable = false
	f.hermActive = true
	f.hermEnable = true
	f.prepareHermesIdentity()
	f.dbPresent = false
	if err := os.RemoveAll(f.path(filepath.Join(testHome, ".openclaw"))); err != nil {
		t.Fatal(err)
	}
	f.prepareProc()

	vs, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), f.job())
	if err != nil || len(vs) != 4 || !allPassed(vs) {
		t.Fatalf("fresh managed activation verifications=%+v err=%v", vs, err)
	}
	wantActions := []string{"daemon-reload", "stop openclaw-gateway.service", "stop clawctl-hermes.service",
		"disable clawctl-hermes.service", "enable openclaw-gateway.service", "start openclaw-gateway.service"}
	if !reflect.DeepEqual(f.actions, wantActions) {
		t.Fatalf("systemctl=%v；要 %v", f.actions, wantActions)
	}
	if !f.openActive || !f.openEnable || f.hermActive || f.hermEnable {
		t.Fatalf("runtime state open=(%v,%v) hermes=(%v,%v)", f.openActive, f.openEnable, f.hermActive, f.hermEnable)
	}
	if f.configCalls != 1 {
		t.Fatalf("gateway.mode configuration calls=%d; want 1", f.configCalls)
	}
	config, err := os.ReadFile(f.path(filepath.Join(testHome, ".openclaw", "openclaw.json")))
	if err != nil || !bytes.Contains(config, []byte(`"mode": "local"`)) {
		t.Fatalf("fresh OpenClaw configuration=%q err=%v", config, err)
	}
}

func TestOpenClawFreshMachineFailureRestoresHermesAndQuarantinesNewState(t *testing.T) {
	f := newExecFixture(t)
	managedNode := filepath.Join(testHome, ".local", "share", "clawctl", "node-runtime", "current", "bin", "node")
	f.install.UnitPath = filepath.Join(testHome, ".config", "systemd", "user", openClawUnit)
	f.install.NodePath = managedNode
	f.install.NpmPath = filepath.Join(filepath.Dir(managedNode), "npm")
	f.install.RunningDir = filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "current",
		"lib", "node_modules", "openclaw")
	f.install.MainPID = 0
	f.install.ProcessMatchesUnit = nil
	f.install.ProcessReason = "unit 沒有 main process"
	f.openActive, f.openEnable = false, false
	f.hermActive, f.hermEnable = true, true
	f.prepareHermesIdentity()
	f.dbPresent = false
	f.mutateDB = true
	f.healthOK = false
	if err := os.RemoveAll(f.path(filepath.Join(testHome, ".openclaw"))); err != nil {
		t.Fatal(err)
	}
	f.prepareProc()
	// Parent ctx must cancel during the health poll (2s sleep), not during the
	// staged --version check. 20ms was enough on a quiet machine but under
	// -race on CI the stage check lost the race and the test saw a single
	// stage failure instead of the Hermes rollback. 1500ms clears stage and
	// still loses to the 2s health sleep.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	job := f.job()
	job.ExecutionTimeout = 1

	vs, err := (openclawExecutor{deps: f.deps()}).Run(ctx, job)
	if err != nil || len(vs) != 5 || vs[len(vs)-1].RuleID != "rollback" || !vs[len(vs)-1].Passed {
		t.Fatalf("verifications=%+v err=%v", vs, err)
	}
	if f.openActive || f.openEnable || !f.hermActive || !f.hermEnable {
		t.Fatalf("runtime state open=(%v,%v) hermes=(%v,%v)", f.openActive, f.openEnable, f.hermActive, f.hermEnable)
	}
	state := filepath.Join(testHome, ".openclaw", "state")
	config := filepath.Join(testHome, ".openclaw", "openclaw.json")
	if _, statErr := os.Stat(f.path(state)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed DB remained active: %v", statErr)
	}
	if _, statErr := os.Stat(f.path(state + ".failed-job-1")); statErr != nil {
		t.Fatalf("failed DB was not quarantined: %v", statErr)
	}
	if _, statErr := os.Stat(f.path(config)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed config remained active: %v", statErr)
	}
	if _, statErr := os.Stat(f.path(config + ".failed-job-1")); statErr != nil {
		t.Fatalf("failed config was not quarantined: %v", statErr)
	}
}

func TestOpenClawConfigurationFailureRestoresPreviousRuntimeAndConfig(t *testing.T) {
	f := newExecFixture(t)
	f.prepareProc()
	f.configErr = errors.New("exit 1")

	vs, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), f.job())
	if err != nil || len(vs) != 2 || vs[0].RuleID != "switch" || vs[0].Passed ||
		vs[1].RuleID != "rollback" || !vs[1].Passed {
		t.Fatalf("verifications=%+v err=%v", vs, err)
	}
	if f.configCalls != 1 || !f.openActive || !f.openEnable || f.hermActive || f.hermEnable {
		t.Fatalf("configCalls=%d runtime open=(%v,%v) hermes=(%v,%v)",
			f.configCalls, f.openActive, f.openEnable, f.hermActive, f.hermEnable)
	}
	config := filepath.Join(testHome, ".openclaw", "openclaw.json")
	if got, readErr := os.ReadFile(f.path(config)); readErr != nil || string(got) != `{"token":"永遠不准印"}` {
		t.Fatalf("restored config=%q err=%v", got, readErr)
	}
}

func TestHealthFailureRollsBackDataAndRemovesUnmanagedDropIn(t *testing.T) {
	f := newExecFixture(t)
	f.prepareProc()
	f.healthOK = false
	f.mutateDB = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	job := f.job()
	job.ExecutionTimeout = 1 // 退回自己再拿一次這個預算；沒有它退回的 health 會等到天荒地老
	vs, err := (openclawExecutor{deps: f.deps()}).Run(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := []string{"daemon-reload", "stop openclaw-gateway.service", "start openclaw-gateway.service", "daemon-reload", "stop openclaw-gateway.service", "start openclaw-gateway.service"}
	if !reflect.DeepEqual(f.actions, wantActions) {
		t.Errorf("systemctl=%v；要 %v", f.actions, wantActions)
	}
	dropIn := filepath.Join(testHome, ".config", "systemd", "user", openClawUnit+".d", openClawDropIn)
	if _, err := os.Stat(f.path(dropIn)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unmanaged previous 的 drop-in 沒刪：%v", err)
	}
	failedDB := filepath.Join(testHome, ".openclaw", "state.failed-job-1")
	if _, err := os.Stat(f.path(failedDB)); err != nil {
		t.Errorf("失敗 DB 沒保留：%v", err)
	}
	if got, _ := os.ReadFile(f.path(filepath.Join(testHome, ".openclaw", "state", "openclaw.sqlite"))); string(got) != "old-db" {
		t.Errorf("DB 沒還原：%q", got)
	}
	failedConfig := filepath.Join(testHome, ".openclaw", "openclaw.json.failed-job-1")
	if _, err := os.Stat(f.path(failedConfig)); err != nil {
		t.Errorf("失敗 config 沒保留：%v", err)
	}
	if got, _ := os.ReadFile(f.path(filepath.Join(testHome, ".openclaw", "openclaw.json"))); string(got) != `{"token":"永遠不准印"}` {
		t.Error("openclaw.json 沒還原")
	}
	if len(vs) != 5 || vs[len(vs)-1].RuleID != "rollback" {
		t.Errorf("驗證缺 rollback：%+v", vs)
	}
}

func TestRollbackToPreviousManagedReleaseRewritesDropIn(t *testing.T) {
	f := newExecFixture(t)
	f.prepareProc()
	f.healthOK = false
	previous := filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "releases", "2026.6.6", "lib", "node_modules", "openclaw")
	f.install.RunningDir = previous
	f.install.ExecStart = testNode + " " + filepath.Join(previous, "dist", "index.js") + " gateway --port 18789"
	dropIn := filepath.Join(testHome, ".config", "systemd", "user", openClawUnit+".d", openClawDropIn)
	want := "[Service]\nExecStart=\nExecStart=" + f.install.ExecStart + "\n"
	f.write(dropIn, []byte(want), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	job := f.job()
	job.ExecutionTimeout = 1
	_, err := (openclawExecutor{deps: f.deps()}).Run(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(f.path(dropIn)); err != nil || string(got) != want {
		t.Errorf("rollback drop-in=%q err=%v；要 %q", got, err, want)
	}
}

func TestReuseAndAlreadyRunningSkipNPMAndStopButStillVerify(t *testing.T) {
	for _, already := range []bool{false, true} {
		t.Run(fmt.Sprintf("already=%v", already), func(t *testing.T) {
			f := newExecFixture(t)
			f.prepareProc()
			if already {
				f.install.RunningDir = filepath.Join(f.targetRelease(), "lib", "node_modules", "openclaw")
			} else {
				pkg := filepath.Join(f.targetRelease(), "lib", "node_modules", "openclaw")
				f.write(filepath.Join(pkg, "package.json"), []byte(`{"version":"2026.6.10"}`), 0o600)
				f.write(filepath.Join(pkg, "dist", "index.js"), []byte("index"), 0o600)
			}
			vs, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), f.job())
			if err != nil {
				t.Fatal(err)
			}
			if f.npmCalls != 0 || already && strings.Contains(strings.Join(f.actions, ","), "stop") {
				t.Errorf("npm=%d actions=%v", f.npmCalls, f.actions)
			}
			if len(vs) != 4 || !allPassed(vs) {
				t.Errorf("重跑仍應做四條驗證：%+v", vs)
			}
		})
	}
}

func TestExecutorContextCancellationReachesNPMAndHealth(t *testing.T) {
	f := newExecFixture(t)
	deps := f.deps()
	npmCanceled := false
	deps.run = func(ctx context.Context, name string, args ...string) (string, string, error) {
		var unwrapped []string
		if name, unwrapped = unwrapScope(name, args); name == testNode && len(unwrapped) > 0 && unwrapped[0] == testNPM {
			<-ctx.Done()
			npmCanceled = true
			return "", "", ctx.Err()
		}
		return "OpenClaw " + testVersion, "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	vs, err := (openclawExecutor{deps: deps}).Run(ctx, f.job())
	if err != nil || !npmCanceled || len(vs) != 1 || vs[0].RuleID != "stage" || vs[0].Passed {
		t.Fatalf("npmCanceled=%v verifications=%+v err=%v", npmCanceled, vs, err)
	}

	healthDeps := f.deps()
	healthDeps.httpGet = func(context.Context, string) (*http.Response, error) {
		return response(http.StatusServiceUnavailable, []byte(`{"ok":false}`)), nil
	}
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer healthCancel()
	if got := healthDeps.health(healthCtx, 18789, "health"); got.Passed || !strings.Contains(got.StderrExcerpt, "deadline") {
		t.Errorf("health deadline 沒傳到迴圈：%+v", got)
	}
}

func TestCleanOpenClawStagingOnlyRemovesStaging(t *testing.T) {
	f := newExecFixture(t)
	releases := filepath.Dir(f.targetRelease())
	for _, name := range []string{".staging-one", ".staging-two", testVersion} {
		if err := os.MkdirAll(f.path(filepath.Join(releases, name)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cleaned, err := cleanOpenClawStaging(testHome, f.root)
	if err != nil || len(cleaned) != 2 {
		t.Fatalf("cleaned=%v err=%v", cleaned, err)
	}
	for _, name := range []string{".staging-one", ".staging-two"} {
		if _, err := os.Stat(f.path(filepath.Join(releases, name))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 沒清掉：%v", name, err)
		}
	}
	if _, err := os.Stat(f.path(filepath.Join(releases, testVersion))); err != nil {
		t.Errorf("正常 release 被清掉：%v", err)
	}
}

func TestParseStageScopeUnits(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{name: "empty"},
		{
			name: "one unit",
			out:  "clawctl-stage-abc-1700000000.scope loaded active running npm install\n",
			want: []string{"clawctl-stage-abc-1700000000.scope"},
		},
		{
			name: "reject unrelated and malformed units",
			out: strings.Join([]string{
				"openclaw-gateway.service loaded active running OpenClaw",
				"clawctl-agent.service loaded active running clawctl agent",
				"session-3.scope loaded active running User session 3",
				"clawctl-stage-prefix-only loaded active running malformed",
			}, "\n"),
		},
		{
			name: "deduplicate and preserve order",
			out: strings.Join([]string{
				"clawctl-stage-first.scope loaded active running first",
				"clawctl-stage-second.scope loaded failed failed second",
				"clawctl-stage-first.scope loaded active running duplicate",
			}, "\n"),
			want: []string{"clawctl-stage-first.scope", "clawctl-stage-second.scope"},
		},
		{
			name: "plain indentation and whitespace",
			out:  "   clawctl-stage-spaced.scope    loaded   inactive   dead    description with spaces\n\t\n",
			want: []string{"clawctl-stage-spaced.scope"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseStageScopeUnits(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseStageScopeUnits()=%v；要 %v", got, tt.want)
			}
		})
	}
}

func TestSweepOrphanStagingStopsScopesBeforeCleaning(t *testing.T) {
	f := newExecFixture(t)
	staging := filepath.Join(filepath.Dir(f.targetRelease()), ".staging-old-job")
	f.write(filepath.Join(staging, "openclaw.tgz"), []byte("partial"), 0o600)
	var calls [][]string
	systemctl := func(_ context.Context, args ...string) (string, string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 1 && args[1] == "list-units" {
			return strings.Join([]string{
				"clawctl-stage-first.scope loaded active running first",
				"clawctl-stage-second.scope loaded active running second",
			}, "\n"), "", nil
		}
		if _, err := os.Stat(f.path(staging)); err != nil {
			t.Errorf("stop scope 前 staging 已被清掉：%v", err)
		}
		return "", "", nil
	}

	reports := sweepOrphanStaging(context.Background(), testHome, f.root, systemctl)
	wantCalls := [][]string{
		{"--user", "list-units", "--type=scope", "--all", "--plain", "--no-legend", "clawctl-stage-*"},
		{"--user", "stop", "clawctl-stage-first.scope"},
		{"--user", "stop", "clawctl-stage-second.scope"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("systemctl calls=%v；要 %v", calls, wantCalls)
	}
	if _, err := os.Stat(f.path(staging)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging 沒清掉：%v", err)
	}
	wantReports := []string{
		"已停掉孤兒 npm scope：clawctl-stage-first.scope",
		"已停掉孤兒 npm scope：clawctl-stage-second.scope",
		"已清理上次中斷留下的 OpenClaw staging：" + staging,
	}
	if !reflect.DeepEqual(reports, wantReports) {
		t.Errorf("reports=%v；要 %v", reports, wantReports)
	}
}

func TestSweepOrphanStagingListFailureStillCleans(t *testing.T) {
	f := newExecFixture(t)
	staging := filepath.Join(filepath.Dir(f.targetRelease()), ".staging-list-failed")
	f.write(filepath.Join(staging, "partial"), []byte("partial"), 0o600)
	reports := sweepOrphanStaging(context.Background(), testHome, f.root,
		func(context.Context, ...string) (string, string, error) {
			return "", "", errors.New("dbus unavailable")
		})
	joined := strings.Join(reports, "\n")
	if !strings.Contains(joined, "掃不到孤兒 scope（不是 0）：dbus unavailable") {
		t.Errorf("list 失敗沒有區分 unknown 與 0：%v", reports)
	}
	if !strings.Contains(joined, "已清理上次中斷留下的 OpenClaw staging："+staging) {
		t.Errorf("list 失敗後沒報 staging 清理：%v", reports)
	}
	if _, err := os.Stat(f.path(staging)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("list 失敗後 staging 沒清掉：%v", err)
	}
}

func TestSweepOrphanStagingContinuesAfterStopFailure(t *testing.T) {
	f := newExecFixture(t)
	releases := filepath.Dir(f.targetRelease())
	// 兩張單各自的 staging：bad 的 scope 停不掉，它的目錄要留著；good 的要清。
	for _, name := range []string{".staging-bad", ".staging-good"} {
		f.write(filepath.Join(releases, name, "openclaw.tgz"), []byte("partial"), 0o600)
	}
	var stops []string
	reports := sweepOrphanStaging(context.Background(), testHome, f.root,
		func(_ context.Context, args ...string) (string, string, error) {
			if len(args) > 1 && args[1] == "list-units" {
				return "clawctl-stage-bad-1700000000.scope loaded active running bad\nclawctl-stage-good-1700000001.scope loaded active running good\n", "", nil
			}
			stops = append(stops, args[2])
			if args[2] == "clawctl-stage-bad-1700000000.scope" {
				return "", "", errors.New("access denied")
			}
			return "", "", nil
		})
	if !reflect.DeepEqual(stops, []string{"clawctl-stage-bad-1700000000.scope", "clawctl-stage-good-1700000001.scope"}) {
		t.Errorf("stop 失敗後沒有繼續：%v", stops)
	}
	want := []string{
		"停不掉孤兒 scope clawctl-stage-bad-1700000000.scope：access denied",
		"已停掉孤兒 npm scope：clawctl-stage-good-1700000001.scope",
		"留著 " + filepath.Join(releases, ".staging-bad") + "：它的 npm scope 還沒停，刪了會被寫回來；下次啟動再清",
		"已清理上次中斷留下的 OpenClaw staging：" + filepath.Join(releases, ".staging-good"),
	}
	if !reflect.DeepEqual(reports, want) {
		t.Errorf("reports=%v；要 %v", reports, want)
	}
	if _, err := os.Stat(f.path(filepath.Join(releases, ".staging-bad"))); err != nil {
		t.Errorf("scope 還活著的 staging 被刪了：%v", err)
	}
	if _, err := os.Stat(f.path(filepath.Join(releases, ".staging-good"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scope 已停的 staging 沒清：%v", err)
	}
}

func TestStageScopeStagingDir(t *testing.T) {
	cases := map[string]string{
		"clawctl-stage-ae43ec64c9c6217052bf980467154cee-1788717467.scope": ".staging-ae43ec64c9c6217052bf980467154cee",
		"clawctl-stage-job-1-1700000000.scope":                            ".staging-job-1",
		"clawctl-stage-one.scope":                                         ".staging-one",
	}
	for unit, want := range cases {
		if got := stageScopeStagingDir(unit); got != want {
			t.Errorf("%s → %q；要 %q", unit, got, want)
		}
	}
}

func TestSweepOrphanStagingReportsTrueZero(t *testing.T) {
	f := newExecFixture(t)
	reports := sweepOrphanStaging(context.Background(), testHome, f.root,
		func(context.Context, ...string) (string, string, error) { return "", "", nil })
	want := []string{"啟動掃描：沒有孤兒 npm scope、沒有 staging 殘骸"}
	if !reflect.DeepEqual(reports, want) {
		t.Errorf("reports=%v；要 %v", reports, want)
	}
}

func TestSweepOrphanStagingExpiredContextDoesNotStop(t *testing.T) {
	f := newExecFixture(t)
	releases := filepath.Dir(f.targetRelease())
	for _, name := range []string{".staging-one", ".staging-two"} {
		f.write(filepath.Join(releases, name, "openclaw.tgz"), []byte("partial"), 0o600)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stops int
	reports := sweepOrphanStaging(ctx, testHome, f.root,
		func(_ context.Context, args ...string) (string, string, error) {
			if len(args) > 1 && args[1] == "list-units" {
				return "clawctl-stage-one.scope loaded active running one\nclawctl-stage-two.scope loaded active running two\n", "", nil
			}
			stops++
			return "", "", nil
		})
	if stops != 0 {
		t.Errorf("ctx 已到期仍呼叫 stop %d 次", stops)
	}
	if joined := strings.Join(reports, "\n"); !strings.Contains(joined, "還有 2 個孤兒 scope 沒處理") {
		t.Errorf("沒有回報未處理數量：%v", reports)
	}
	// 沒輪到停的 scope，它們的 staging 不准刪（刪了 npm 會寫回來）。
	for _, name := range []string{".staging-one", ".staging-two"} {
		if _, err := os.Stat(f.path(filepath.Join(releases, name))); err != nil {
			t.Errorf("%s 被刪了，但它的 scope 沒停：%v", name, err)
		}
	}
}

func TestOpenClawAfterSucceededRetainsOnlyRollbackRelevantFiles(t *testing.T) {
	f := newExecFixture(t)
	d := f.deps()
	job := f.job()
	currentRelease := f.targetRelease()
	previousRelease := filepath.Join(d.releasesDir(), "2026.6.6")
	obsoleteRelease := filepath.Join(d.releasesDir(), "2026.5.0")
	for _, release := range []string{currentRelease, previousRelease, obsoleteRelease} {
		f.write(filepath.Join(release, "marker"), []byte(filepath.Base(release)), 0o600)
	}
	if err := d.setCurrent(currentRelease); err != nil {
		t.Fatal(err)
	}
	ownSnapshot := filepath.Join(d.snapshotsDir(), safeJobID(job.JobID))
	oldSnapshot := filepath.Join(d.snapshotsDir(), "older-job")
	previous := previousRelease + string(filepath.Separator) + filepath.Join("lib", "node_modules", "openclaw")
	f.write(filepath.Join(ownSnapshot, "previous.json"),
		[]byte(fmt.Sprintf(`{"running_dir":%q}`, previous)), 0o600)
	f.write(filepath.Join(ownSnapshot, "keep"), []byte("own"), 0o600)
	f.write(filepath.Join(oldSnapshot, "remove"), []byte("old"), 0o600)

	failed32 := strings.Repeat("a", 32)
	for _, path := range []string{
		filepath.Join(testHome, ".openclaw", "state.failed-older-job", "db"),
		filepath.Join(testHome, ".openclaw", "openclaw.json.failed-"+failed32),
	} {
		f.write(path, []byte("failed"), 0o600)
	}
	unrelated := filepath.Join(testHome, ".openclaw", "state.backup-x")
	wrongShape := filepath.Join(testHome, ".openclaw", "state.failed-失敗證據")
	f.write(unrelated, []byte("keep"), 0o600)
	f.write(wrongShape, []byte("keep"), 0o600)

	reports := (openclawExecutor{deps: d}).AfterSucceeded(context.Background(), job)
	if len(reports) == 0 {
		t.Fatal("保留期有清東西卻沒有回報任何一行")
	}
	for _, logical := range []string{oldSnapshot, obsoleteRelease,
		filepath.Join(testHome, ".openclaw", "state.failed-older-job"),
		filepath.Join(testHome, ".openclaw", "openclaw.json.failed-"+failed32)} {
		if _, err := os.Lstat(f.path(logical)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("該清掉的 %s 還在：%v", logical, err)
		}
	}
	for _, logical := range []string{ownSnapshot, currentRelease, previousRelease, unrelated, wrongShape} {
		if _, err := os.Lstat(f.path(logical)); err != nil {
			t.Errorf("該保留的 %s 不見了：%v", logical, err)
		}
	}
	entries, err := os.ReadDir(f.path(d.releasesDir()))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	sort.Strings(got)
	want := []string{testVersion, "2026.6.6"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("保留後 releases=%v；要 current 與本張 previous %v", got, want)
	}
}

func TestOpenClawFailedJobDoesNotRunRetention(t *testing.T) {
	f := newExecFixture(t)
	d := f.deps()
	job := f.job()
	job.Revision = 51
	f.install.RunningDir = filepath.Join(f.targetRelease(), "lib", "node_modules", "openclaw")
	f.prepareProc()
	f.write(filepath.Join(f.targetRelease(), "marker"), []byte("current"), 0o600)
	f.write(filepath.Join(d.snapshotsDir(), "old-job", "snapshot"), []byte("keep"), 0o600)
	f.write(filepath.Join(testHome, ".openclaw", "state.failed-old-job", "db"), []byte("keep"), 0o600)
	f.write(filepath.Join(d.releasesDir(), "old-release", "marker"), []byte("keep"), 0o600)
	if err := d.setCurrent(f.targetRelease()); err != nil {
		t.Fatal(err)
	}
	before := fsFingerprint(t, f.root)
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	runScript(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1),
		verificationStep(job.JobID, nil), verificationStep(job.JobID, nil),
		verificationStep(job.JobID, nil), verificationStep(job.JobID, nil),
		eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(deploy.Failed), false), noJobStep(),
	}, journal, openclawExecutor{deps: d})
	if after := fsFingerprint(t, f.root); !reflect.DeepEqual(before, after) {
		t.Errorf("Hub 判 failed 卻動了保留物：\nbefore=%v\nafter=%v", before, after)
	}
}

func TestOpenClawRetentionSkipsAllReleasesWhenCurrentIsBroken(t *testing.T) {
	f := newExecFixture(t)
	d := f.deps()
	for _, name := range []string{"one", "two", "three"} {
		f.write(filepath.Join(d.releasesDir(), name, "marker"), []byte(name), 0o600)
	}
	current := filepath.Join(d.openClawRoot(), "current")
	if err := os.Symlink(filepath.Join("..", "outside"), f.path(current)); err != nil {
		t.Fatal(err)
	}
	before := fsFingerprint(t, f.path(d.releasesDir()))
	reports := (openclawExecutor{deps: d}).AfterSucceeded(context.Background(), f.job())
	after := fsFingerprint(t, f.path(d.releasesDir()))
	if !reflect.DeepEqual(before, after) {
		t.Errorf("current 壞掉仍清了 releases：before=%v after=%v", before, after)
	}
	if !strings.Contains(strings.Join(reports, "\n"), "沒清 releases：current 讀不到") {
		t.Errorf("current 壞掉沒有明確回報：%v", reports)
	}
}

// ⚠ 守的是：health 等待時 execution ctx 取消後，退回**不能**用那個已取消的 ctx。
// 真的 exec.CommandContext 對已取消或到期的 ctx 會直接失敗，daemon-reload／stop／start 一個都跑不了，
// 機器就停在「新 drop-in ＋ 起不來的 gateway」。原本的假 systemctl 不看 ctx，所以看不到這個洞；
// 這裡的假 systemctl 跟真的一樣尊重 ctx。
func TestRollbackStillRunsAfterExecutionContextCanceled(t *testing.T) {
	f := newExecFixture(t)
	f.prepareProc()
	f.healthOK = false
	deps := f.deps()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps = cancelOnFirstRetry(deps, cancel)
	realSystemctl := deps.systemctl
	deps.systemctl = func(ctx context.Context, args ...string) (string, string, error) {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		return realSystemctl(ctx, args...)
	}
	job := f.job()
	job.ExecutionTimeout = 60
	vs, err := (openclawExecutor{deps: deps}).Run(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("health 等待時沒有取消執行預算")
	}
	wantActions := []string{"daemon-reload", "stop openclaw-gateway.service", "start openclaw-gateway.service",
		"daemon-reload", "stop openclaw-gateway.service", "start openclaw-gateway.service"}
	if !reflect.DeepEqual(f.actions, wantActions) {
		t.Errorf("預算用完後退回沒有真的跑：systemctl=%v；要 %v", f.actions, wantActions)
	}
	last := vs[len(vs)-1]
	if last.RuleID != "rollback" || strings.Contains(last.StderrExcerpt, "daemon-reload：") ||
		strings.Contains(last.StderrExcerpt, "stop：") || strings.Contains(last.StderrExcerpt, "start：") {
		t.Errorf("退回的 systemctl 指令拿到了到期的 ctx：%+v", last)
	}
	dropIn := filepath.Join(testHome, ".config", "systemd", "user", openClawUnit+".d", openClawDropIn)
	if _, err := os.Stat(f.path(dropIn)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("退回後 drop-in 還在：%v", err)
	}
}

// Cancel only after the switch has completed and health starts retrying. A short
// wall-clock timeout can expire during staging on a busy machine, before the
// rollback scenario this test intends to exercise.
func cancelOnFirstRetry(deps execDeps, cancel context.CancelFunc) execDeps {
	canceled := false
	deps.sleep = func(ctx context.Context, delay time.Duration) error {
		if !canceled {
			canceled = true
			cancel()
			return ctx.Err()
		}
		// The rollback health probe is expected to fail; keep the test independent
		// of real-time waits and filesystem stalls on the test host.
		return context.DeadlineExceeded
	}
	return deps
}

// ⚠ 守的是 artifact.url 必須是相對於 Hub 的路徑：agent 只從自己的 Hub 抓，spec 裡塞一個
// 絕對 URL 不該讓 agent 跑去別的地方下載（就算 digest 會擋，也不該連出去）。
func TestGateRejectsAbsoluteArtifactURL(t *testing.T) {
	f := newExecFixture(t)
	job := f.job()
	job.Spec = []byte(strings.Replace(string(job.Spec), `"url":"/v1/artifacts/`, `"url":"https://elsewhere.example/v1/artifacts/`, 1))
	before := fsFingerprint(t, f.root)
	_, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), job)
	var rejected *rejectError
	if !errors.As(err, &rejected) || rejected.Code != deploy.PreconditionFailed || !strings.Contains(rejected.Detail, "artifact.url") {
		t.Fatalf("絕對 URL 的 artifact 沒被閘門拒：%v", err)
	}
	if !reflect.DeepEqual(before, fsFingerprint(t, f.root)) {
		t.Error("閘門拒單卻寫了檔案")
	}
}

// systemd 在 fork 之後、execve 之前把子行程改名成 "(node)"（rename_process_from_path）。
// 2026-09-06 金絲雀 f0537321 就是 start 一回來就讀 /proc/<MainPID>/cmdline，讀到這個佔位名，
// 被判 process_cmdline 不過，然後把一個其實健康的 release 退回了。
func placeholderCmdline() []byte { return []byte("(node)\x00") }

func TestProcessCmdlineIsReadAfterHealthNotAtSystemdForkPlaceholder(t *testing.T) {
	f := newExecFixture(t)
	f.write("/proc/4321/cmdline", placeholderCmdline(), 0o600)
	deps := f.deps()
	base := deps.httpGet
	deps.httpGet = func(ctx context.Context, rawURL string) (*http.Response, error) {
		if strings.Contains(rawURL, "/health") {
			// gateway 能回 health 時 execve 早就發生了：這時 cmdline 才是 gateway 的 argv。
			f.prepareProc()
		}
		return base(ctx, rawURL)
	}
	vs, err := (openclawExecutor{deps: deps}).Run(context.Background(), f.job())
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 4 || !allPassed(vs) {
		t.Fatalf("health 過了之後讀到的 cmdline 才算數，四條應全過：%+v", vs)
	}
	var rules []string
	for _, v := range vs {
		rules = append(rules, v.RuleID)
	}
	if want := []string{"unit_execstart", "health", "process_cmdline", "version"}; !reflect.DeepEqual(rules, want) {
		t.Errorf("驗證順序=%v；要 %v（process_cmdline 必須在 health 之後）", rules, want)
	}
	if got, want := f.actions, []string{"daemon-reload", "stop openclaw-gateway.service", "start openclaw-gateway.service"}; !reflect.DeepEqual(got, want) {
		t.Errorf("健康的 release 不該被退回：systemctl=%v；要 %v", got, want)
	}
}

func TestProcessCmdlineWaitsWhilePlaceholderPersistsAfterHealth(t *testing.T) {
	f := newExecFixture(t)
	f.write("/proc/4321/cmdline", placeholderCmdline(), 0o600)
	deps := f.deps()
	sleeps := 0
	deps.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps++
		f.prepareProc() // 佔位名是暫態：等一下再讀就是真的 argv
		return nil
	}
	vs, err := (openclawExecutor{deps: deps}).Run(context.Background(), f.job())
	if err != nil {
		t.Fatal(err)
	}
	if sleeps == 0 {
		t.Fatal("佔位名 (node) 應該等一下再讀，不是立刻判失敗")
	}
	if len(vs) != 4 || !allPassed(vs) {
		t.Fatalf("等到真的 argv 之後四條應全過：%+v", vs)
	}
}

func TestProcessCmdlineFailsFastOnForeignArgvAndRollsBack(t *testing.T) {
	f := newExecFixture(t)
	foreign := testNode + "\x00/opt/openclaw/dist/index.js\x00gateway\x00--port\x0018789\x00"
	f.write("/proc/4321/cmdline", []byte(foreign), 0o600)
	deps := f.deps()
	sleeps := 0
	deps.sleep = func(ctx context.Context, d time.Duration) error { sleeps++; return nil }
	job := f.job()
	job.ExecutionTimeout = 1
	vs, err := (openclawExecutor{deps: deps}).Run(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if sleeps != 0 {
		t.Errorf("跑的是別的程式（舊 index.js），不是暫態，不該等：sleeps=%d", sleeps)
	}
	var process *model.JobVerificationRequest
	for i := range vs {
		if vs[i].RuleID == "process_cmdline" {
			process = &vs[i]
		}
	}
	if process == nil || process.Passed || !strings.Contains(process.StdoutExcerpt, "/opt/openclaw/dist/index.js") {
		t.Fatalf("process_cmdline 應失敗且證據要印出實際 argv：%+v", process)
	}
	if vs[len(vs)-1].RuleID != "rollback" || len(f.actions) != 6 {
		t.Errorf("跑的不是新 release 就要退回：%+v actions=%v", vs, f.actions)
	}
}

func TestRollbackRestoresCurrentSymlink(t *testing.T) {
	current := filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "current")
	t.Run("previous unmanaged → current 拿掉", func(t *testing.T) {
		f := newExecFixture(t)
		f.prepareProc()
		f.healthOK = false
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		job := f.job()
		job.ExecutionTimeout = 60
		if _, err := (openclawExecutor{deps: cancelOnFirstRetry(f.deps(), cancel)}).Run(ctx, job); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("health 等待時沒有取消執行預算")
		}
		if got, err := os.Readlink(f.path(current)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("退回到 releases 以外的安裝後 current 還指著失敗的 release：%q err=%v", got, err)
		}
	})
	t.Run("previous managed → current 指回去", func(t *testing.T) {
		f := newExecFixture(t)
		f.prepareProc()
		f.healthOK = false
		previous := filepath.Join(testHome, ".local", "share", "clawctl", "openclaw", "releases", "2026.6.6", "lib", "node_modules", "openclaw")
		f.install.RunningDir = previous
		f.install.ExecStart = testNode + " " + filepath.Join(previous, "dist", "index.js") + " gateway --port 18789"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		job := f.job()
		job.ExecutionTimeout = 60
		if _, err := (openclawExecutor{deps: cancelOnFirstRetry(f.deps(), cancel)}).Run(ctx, job); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("health 等待時沒有取消執行預算")
		}
		if got, err := os.Readlink(f.path(current)); err != nil || got != filepath.Join("releases", "2026.6.6") {
			t.Errorf("current=%q err=%v；要 releases/2026.6.6", got, err)
		}
	})
}

// agent unit 有 MemoryMax=256M、CPUQuota=20%（ops/clawctl-agent.service）。npm 在 agent 的 cgroup 裡跑會整套繼承：
// sampleagent4 2026-09-06 npm 9 裝 2026.5.26，node 照 cgroup 上限把 V8 heap 縮到 252 MB，12 分鐘後 "Reached heap limit"（43c791e4）。
func TestStageRunsNPMInItsOwnScopeNotTheAgentCgroup(t *testing.T) {
	f := newExecFixture(t)
	f.prepareProc()
	vs, err := (openclawExecutor{deps: f.deps()}).Run(context.Background(), f.job())
	if err != nil || len(vs) != 4 || !allPassed(vs) {
		t.Fatalf("verifications=%+v err=%v", vs, err)
	}
	if f.npmCalls != 1 || len(f.scopeRuns) != 1 {
		t.Fatalf("npm 要在自己的 scope 裡跑恰好一次：npmCalls=%d scopeRuns=%v", f.npmCalls, f.scopeRuns)
	}
	got := f.scopeRuns[0]
	wantPrefix := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect"}
	if len(got) < len(wantPrefix)+5 || !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Errorf("scope 包裝=%v；要以 %v 開頭", got, wantPrefix)
	}
	unit := got[len(wantPrefix)]
	if !strings.HasPrefix(unit, "--unit=clawctl-stage-job-1") {
		t.Errorf("scope unit 要以工作單命名：%q", unit)
	}
	if got[len(wantPrefix)+1] != "--" || got[len(wantPrefix)+2] != testNode ||
		got[len(wantPrefix)+3] != testNPM || got[len(wantPrefix)+4] != "install" {
		t.Errorf("-- 之後要由配套 Node 執行 npm install：%v", got[len(wantPrefix)+1:])
	}
}

// unwrapScope 拆開 runInOwnScope 的包裝（systemd-run --user --scope … -- <cmd> <args>）；不是包裝就原樣回。
func unwrapScope(name string, args []string) (string, []string) {
	if name != "systemd-run" {
		return name, args
	}
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			return args[i+1], args[i+2:]
		}
	}
	return name, args
}
