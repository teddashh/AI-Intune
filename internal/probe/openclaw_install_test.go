package probe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseGatewayExecStartRealLayouts(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		node       string
		dir        string
		gatewayArg []string
	}{
		{
			name: "samplehub1",
			raw:  "ExecStart={ path=/usr/bin/node ; argv[]=/usr/bin/node /home/example-user/.local/node_modules/openclaw/dist/index.js gateway --port 18789 ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }",
			node: "/usr/bin/node", dir: "/home/example-user/.local/node_modules/openclaw",
			gatewayArg: []string{"gateway", "--port", "18789"},
		},
		{
			name: "sampleagent2",
			raw:  "ExecStart={ path=/usr/bin/node ; argv[]=/usr/bin/node /usr/lib/node_modules/openclaw/dist/index.js gateway --port 18789 ; ignore_errors=no ; start_time=[n/a] }",
			node: "/usr/bin/node", dir: "/usr/lib/node_modules/openclaw",
			gatewayArg: []string{"gateway", "--port", "18789"},
		},
		{
			name: "sampleagent3",
			raw:  "ExecStart={ path=/home/example-user-c/.local/node24/bin/node ; argv[]=/home/example-user-c/.local/node24/bin/node /home/example-user-c/.local/node24/lib/node_modules/openclaw/dist/index.js gateway --port 18789 ; ignore_errors=no ; start_time=[n/a] }",
			node: "/home/example-user-c/.local/node24/bin/node", dir: "/home/example-user-c/.local/node24/lib/node_modules/openclaw",
			gatewayArg: []string{"gateway", "--port", "18789"},
		},
		{
			name: "sampleagent4",
			raw:  "ExecStart={ path=/usr/bin/node ; argv[]=/usr/bin/node /home/example-user-b/.local/lib/node_modules/openclaw/dist/index.js gateway --port 18789 ; ignore_errors=no ; start_time=[n/a] }",
			node: "/usr/bin/node", dir: "/home/example-user-b/.local/lib/node_modules/openclaw",
			gatewayArg: []string{"gateway", "--port", "18789"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGatewayExecStart(tt.raw)
			if err != nil {
				t.Fatalf("解析失敗：%v", err)
			}
			if got.nodePath != tt.node || got.runningDir != tt.dir || !reflect.DeepEqual(got.args, tt.gatewayArg) {
				t.Fatalf("解析結果 = %+v", got)
			}
		})
	}
}

func TestParseGatewayExecStartUsesLastEntry(t *testing.T) {
	raw := "ExecStart={ path=/old/node ; argv[]=/old/node /old/openclaw/dist/index.js gateway ; ignore_errors=no } " +
		"{ path= ; argv[]= ; ignore_errors=no } " +
		"{ path=/new/node ; argv[]=/new/node /new/openclaw/dist/index.js gateway --port 18789 ; ignore_errors=no }"
	got, err := parseGatewayExecStart(raw)
	if err != nil {
		t.Fatalf("解析失敗：%v", err)
	}
	if got.nodePath != "/new/node" || got.runningDir != "/new/openclaw" {
		t.Fatalf("沒有拿最後一條 ExecStart：%+v", got)
	}
}

func TestParseGatewayExecStartRejectsWrongShapeAndEmpty(t *testing.T) {
	bad := "ExecStart={ path=/usr/bin/openclaw ; argv[]=/usr/bin/openclaw gateway ; ignore_errors=no }"
	got, err := parseGatewayExecStart(bad)
	if err == nil || !strings.Contains(err.Error(), "/usr/bin/openclaw gateway") {
		t.Fatalf("錯誤沒有保留原文：結果 %+v，錯誤 %v", got, err)
	}
	if _, err := parseGatewayExecStart(""); err == nil || !strings.Contains(err.Error(), "空") {
		t.Fatalf("空字串錯誤 = %v", err)
	}
}

func TestDiscoverInstallComplete(t *testing.T) {
	fx := newInstallFixture(t, 4242, "/srv/openclaw/dist/index.js")
	fx.write("/srv/openclaw/package.json", `{"name":"openclaw","version":"2026.6.10"}`, 0o644)
	fx.write("/opt/node24/bin/npm", "fake", 0o755)
	fx.write("/proc/4242/cmdline", "/opt/node24/bin/node\x00/srv/openclaw/dist/index.js\x00gateway\x00--port\x0018789\x00", 0o644)
	fx.mkdir("/home/test/.local/share/clawctl/openclaw/releases", 0o755)
	if err := os.Symlink("releases/2026.6.10", fx.path("/home/test/.local/share/clawctl/openclaw/current")); err != nil {
		t.Fatalf("建立 current 測試 symlink：%v", err)
	}
	fx.run = func(_ context.Context, timeout time.Duration, name string, args ...string) (string, string, error) {
		if timeout != 10*time.Second || !reflect.DeepEqual(args, []string{"--version"}) {
			t.Fatalf("指令 %s 的 timeout/args 不對：%s %v", name, timeout, args)
		}
		switch name {
		case "/opt/node24/bin/node":
			return "v24.15.0\n", "", nil
		case "/opt/node24/bin/npm":
			return "11.12.1\n", "", nil
		default:
			return "", "", fmt.Errorf("不該執行 %s", name)
		}
	}

	got := discoverInstall(context.Background(), fx.deps())
	if fx.systemctlCalls != 1 {
		t.Fatalf("systemctl 呼叫了 %d 次，預期一次問完", fx.systemctlCalls)
	}
	if !got.UnitFound || got.UnitReason != "" || got.UnitPath != "/home/test/.config/systemd/user/openclaw-gateway.service" || got.KillMode != "control-group" {
		t.Fatalf("unit 欄位不對：%+v", got)
	}
	if got.NRestarts == nil || *got.NRestarts != 3 || got.ActiveEnterAt == nil || got.ActiveEnterAt.Unix() != 1788324026 || got.MainPID != 4242 {
		t.Fatalf("systemd 計數/時間/PID 不對：%+v", got)
	}
	if !reflect.DeepEqual(got.DropInPaths, []string{"/one.conf", "/two.conf"}) {
		t.Fatalf("drop-in = %v", got.DropInPaths)
	}
	if got.NodePath != "/opt/node24/bin/node" || got.NodeVersion != "v24.15.0" {
		t.Fatalf("node 欄位不對：%+v", got)
	}
	if got.ExecStart != "/opt/node24/bin/node /srv/openclaw/dist/index.js gateway --port 18789" || !reflect.DeepEqual(got.GatewayArgs, []string{"gateway", "--port", "18789"}) {
		t.Fatalf("ExecStart/GatewayArgs 不對：%+v", got)
	}
	if got.NpmPath != "/opt/node24/bin/npm" || got.NpmVersion != "11.12.1" || got.NpmReason != "" {
		t.Fatalf("npm 欄位不對：%+v", got)
	}
	if got.RunningDir != "/srv/openclaw" || !got.RunningDirExists || got.RunningDirOwner == "" || got.RunningDirWritable == nil || !*got.RunningDirWritable || got.RunningDirVersion != "2026.6.10" {
		t.Fatalf("執行目錄欄位不對：%+v", got)
	}
	if got.ProcessIndexJS != "/srv/openclaw/dist/index.js" || got.ProcessMatchesUnit == nil || !*got.ProcessMatchesUnit {
		t.Fatalf("process 欄位不對：%+v", got)
	}
	if got.RunningDirReason != "" || got.ProcessReason != "" || got.NodeVersionReason != "" || got.DiskFreeReason != "" {
		t.Fatalf("完整觀測不該有失敗 Reason：%+v", got)
	}
	if got.ReleasesDir != "/home/test/.local/share/clawctl/openclaw/releases" || !got.ReleasesPresent || got.CurrentLink != "releases/2026.6.10" {
		t.Fatalf("release 版面不對：%+v", got)
	}
	if !got.DiskFreeMeasured || got.DiskFreeBytes <= 0 {
		t.Fatalf("磁碟欄位不對：%+v", got)
	}
}

func TestDiscoverInstallUnitNotFoundDoesNotInventRuntime(t *testing.T) {
	fx := newInstallFixture(t, 4242, "/fake/dist/index.js")
	fx.systemctlOut = "FragmentPath=\nExecStart={ path=/fake/node ; argv[]=/fake/node /fake/dist/index.js gateway ; ignore_errors=no }\nMainPID=4242\n"
	called := false
	fx.run = func(context.Context, time.Duration, string, ...string) (string, string, error) {
		called = true
		return "fake", "", nil
	}
	got := discoverInstall(context.Background(), fx.deps())
	if got.UnitFound || !strings.Contains(got.UnitReason, "FragmentPath") {
		t.Fatalf("不存在的 unit = %+v", got)
	}
	if got.NodePath != "" || got.RunningDir != "" || got.MainPID != 0 || got.ProcessMatchesUnit != nil || called {
		t.Fatalf("不存在的 unit 卻生出 runtime 事實：%+v", got)
	}
}

func TestDiscoverInstallSystemctlFailureKeepsInstallWithReason(t *testing.T) {
	fx := newInstallFixture(t, 4242, "/fake/dist/index.js")
	deps := fx.deps()
	deps.systemctl = func(context.Context, time.Duration, ...string) (string, string, error) {
		return "", "Failed to connect to bus", errors.New("exit status 1")
	}
	got := discoverInstall(context.Background(), deps)
	if got == nil || got.UnitFound || !strings.Contains(got.UnitReason, "Failed to connect to bus") {
		t.Fatalf("systemctl 失敗沒有留下非 nil Install 與理由：%+v", got)
	}
	if got.NodePath != "" || got.RunningDir != "" || got.MainPID != 0 || got.ProcessMatchesUnit != nil {
		t.Fatalf("systemctl 失敗卻生出 runtime 事實：%+v", got)
	}
	// ⚠ release 版面與磁碟是 stat/statfs，跟 user bus 無關。systemctl 失敗就把
	// 它們留成零值，DiskFreeMeasured=false 還算誠實，但 ReleasesPresent=false
	// 沒有 Reason 可帶，會讓「沒量」長得跟「量到不存在」一模一樣。
	if got.ReleasesDir == "" || !got.DiskFreeMeasured {
		t.Fatalf("systemctl 失敗不該連帶放棄 release/disk 這兩項獨立量測：%+v", got)
	}
}

func TestDiscoverInstallReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 的 Access(W_OK) 不適合驗證 mode 0555")
	}
	fx := newInstallFixture(t, 0, "/srv/openclaw/dist/index.js")
	fx.write("/srv/openclaw/package.json", `{"version":"2026.6.6"}`, 0o644)
	if err := os.Chmod(fx.path("/srv/openclaw"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fx.path("/srv/openclaw"), 0o755) })
	got := discoverInstall(context.Background(), fx.deps())
	if got.RunningDirWritable == nil || *got.RunningDirWritable {
		t.Fatalf("0555 目錄的 writable = %v", got.RunningDirWritable)
	}
}

func TestDiscoverInstallProcessMismatchAndNoMainPID(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		fx := newInstallFixture(t, 99, "/srv/openclaw/dist/index.js")
		fx.write("/srv/openclaw/package.json", `{"version":"1"}`, 0o644)
		fx.write("/proc/99/cmdline", "/opt/node24/bin/node\x00/old/openclaw/dist/index.js\x00gateway\x00", 0o644)
		got := discoverInstall(context.Background(), fx.deps())
		if got.ProcessMatchesUnit == nil || *got.ProcessMatchesUnit || got.ProcessIndexJS != "/old/openclaw/dist/index.js" {
			t.Fatalf("process mismatch = %+v", got)
		}
	})
	t.Run("no main pid", func(t *testing.T) {
		fx := newInstallFixture(t, 0, "/srv/openclaw/dist/index.js")
		fx.write("/srv/openclaw/package.json", `{"version":"1"}`, 0o644)
		got := discoverInstall(context.Background(), fx.deps())
		if got.ProcessMatchesUnit != nil || !strings.Contains(got.ProcessReason, "沒有 main process") {
			t.Fatalf("MainPID=0 的 process = %+v", got)
		}
	})
}

func TestDiscoverInstallNodeFailureKeepsOtherFacts(t *testing.T) {
	fx := newInstallFixture(t, 0, "/srv/openclaw/dist/index.js")
	fx.write("/srv/openclaw/package.json", `{"version":"2026.6.6"}`, 0o644)
	fx.run = func(_ context.Context, _ time.Duration, name string, _ ...string) (string, string, error) {
		if name == "/opt/node24/bin/node" {
			return "", "permission denied", errors.New("exit status 1")
		}
		return "", "", errors.New("unexpected")
	}
	got := discoverInstall(context.Background(), fx.deps())
	if !strings.Contains(got.NodeVersionReason, "permission denied") || got.RunningDirVersion != "2026.6.6" || !got.UnitFound {
		t.Fatalf("node 失敗抹掉其他欄位：%+v", got)
	}
}

func TestDiscoverInstallNPMReasonNamesBothSearchLocations(t *testing.T) {
	fx := newInstallFixture(t, 0, "/srv/openclaw/dist/index.js")
	fx.write("/srv/openclaw/package.json", `{"version":"1"}`, 0o644)
	got := discoverInstall(context.Background(), fx.deps())
	for _, want := range []string{"/opt/node24/bin/npm", "agent PATH", "/agent/bin:/usr/local/bin"} {
		if !strings.Contains(got.NpmReason, want) {
			t.Errorf("NpmReason 沒提到 %q：%s", want, got.NpmReason)
		}
	}
}

func TestOpenClawDBDirUsesTheSameLayoutOrderAsObservation(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".openclaw", "state")
	tasks := filepath.Join(home, ".openclaw", "tasks")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tasks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasks, "runs.sqlite"), []byte("split"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dir, layout, ok := OpenClawDBDir(home); !ok || dir != tasks || layout != "split" {
		t.Fatalf("split = (%q,%q,%v)", dir, layout, ok)
	}
	if err := os.WriteFile(filepath.Join(state, "openclaw.sqlite"), []byte("consolidated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dir, layout, ok := OpenClawDBDir(home); !ok || dir != state || layout != "consolidated" {
		t.Fatalf("consolidated priority = (%q,%q,%v)", dir, layout, ok)
	}
}

type installFixture struct {
	t              *testing.T
	root           string
	systemctlOut   string
	run            installRunFunc
	agentPath      string
	systemctlCalls int
}

func newInstallFixture(t *testing.T, pid int, indexJS string) *installFixture {
	t.Helper()
	fx := &installFixture{t: t, root: t.TempDir(), agentPath: "/agent/bin:/usr/local/bin"}
	fx.mkdir("/home/test/.local/share", 0o755)
	fx.systemctlOut = fmt.Sprintf("FragmentPath=/home/test/.config/systemd/user/openclaw-gateway.service\nDropInPaths=/one.conf /two.conf\nExecStart={ path=/opt/node24/bin/node ; argv[]=/opt/node24/bin/node %s gateway --port 18789 ; ignore_errors=no ; start_time=[n/a] }\nKillMode=control-group\nNRestarts=3\nActiveEnterTimestamp=@1788324026\nMainPID=%d\n", indexJS, pid)
	fx.run = func(_ context.Context, _ time.Duration, name string, _ ...string) (string, string, error) {
		if name == "/opt/node24/bin/node" {
			return "v24.15.0", "", nil
		}
		return "", "", errors.New("not found")
	}
	return fx
}

func (f *installFixture) deps() installDeps {
	return installDeps{
		home: "/home/test", procRoot: f.path("/proc"), fsRoot: f.root, agentPath: f.agentPath,
		run: f.run,
		systemctl: func(_ context.Context, timeout time.Duration, args ...string) (string, string, error) {
			f.systemctlCalls++
			if timeout != 10*time.Second {
				f.t.Fatalf("systemctl timeout = %s", timeout)
			}
			want := "--user show openclaw-gateway.service -p FragmentPath -p DropInPaths -p ExecStart -p KillMode -p NRestarts -p ActiveEnterTimestamp -p MainPID"
			if strings.Join(args, " ") != want {
				f.t.Fatalf("systemctl args = %q", strings.Join(args, " "))
			}
			return f.systemctlOut, "", nil
		},
	}
}

func (f *installFixture) path(logical string) string {
	return filepath.Join(f.root, strings.TrimPrefix(filepath.Clean(logical), string(filepath.Separator)))
}

func (f *installFixture) mkdir(logical string, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(f.path(logical), mode); err != nil {
		f.t.Fatalf("mkdir %s：%v", logical, err)
	}
}

func (f *installFixture) write(logical, contents string, mode os.FileMode) {
	f.t.Helper()
	path := f.path(logical)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("mkdir parent %s：%v", logical, err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		f.t.Fatalf("write %s：%v", logical, err)
	}
}
