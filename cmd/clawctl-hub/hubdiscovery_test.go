package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	old, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func operatorConfigFixture(t *testing.T, raw string, mode os.FileMode) string {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	dir := filepath.Join(configHome, "clawctl")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, operatorConfigFilename)
	if err := os.WriteFile(path, []byte(raw), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorHubDiscoveryPrecedenceAndNormalization(t *testing.T) {
	operatorConfigFixture(t, `{"hub_url":"http://100.64.0.20:8787"}`, 0o600)
	t.Setenv(operatorHubURLEnv, "http://100.64.0.10:8787/")
	got, err := discoverOperatorHubURL()
	if err != nil || got != "http://100.64.0.10:8787" {
		t.Fatalf("discovery=%q err=%v", got, err)
	}
}

func TestOperatorHubDiscoverySelectedEnvironmentNeverFallsBack(t *testing.T) {
	operatorConfigFixture(t, `{"hub_url":"http://100.64.0.20:8787"}`, 0o600)
	for _, raw := range []string{"", "   ", "http://127.0.0.1:8787", "https://100.64.0.10:8787", "http://user:secret@100.64.0.10:8787"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(operatorHubURLEnv, raw)
			got, err := discoverOperatorHubURL()
			if err == nil || got != "" {
				t.Fatalf("selected bad env discovery=%q err=%v", got, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("discovery leaked malformed URL userinfo: %v", err)
			}
		})
	}
}

func TestOperatorHubDiscoveryReadsDedicatedStrictConfig(t *testing.T) {
	unsetEnvForTest(t, operatorHubURLEnv)
	path := operatorConfigFixture(t, `{"hub_url":"http://100.64.0.20:8787/"}`, 0o600)
	got, err := discoverOperatorHubURL()
	if err != nil || got != "http://100.64.0.20:8787" {
		t.Fatalf("discovery=%q err=%v", got, err)
	}
	want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "clawctl", "operator.json")
	if path != want {
		t.Fatalf("config path=%q want=%q", path, want)
	}
}

func TestOperatorHubDiscoveryAcceptsReadOnlySharedConfigButNotSharedWrites(t *testing.T) {
	unsetEnvForTest(t, operatorHubURLEnv)
	operatorConfigFixture(t, `{"hub_url":"http://100.64.0.20:8787"}`, 0o644)
	if got, err := discoverOperatorHubURL(); err != nil || got != "http://100.64.0.20:8787" {
		t.Fatalf("0644 non-secret config discovery=%q err=%v", got, err)
	}
}

func TestOperatorHubDiscoveryIgnoresMachineAndServerInputs(t *testing.T) {
	unsetEnvForTest(t, operatorHubURLEnv)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CLAWCTL_DB", filepath.Join(t.TempDir(), "tempting.sqlite"))
	t.Setenv("CLAWCTL_PUBLIC_URL", "http://100.64.0.31:8787")
	t.Setenv("CLAWCTL_CONFIG", filepath.Join(t.TempDir(), "agent.json"))
	if err := os.WriteFile(os.Getenv("CLAWCTL_CONFIG"), []byte(`{"hub_url":"http://100.64.0.32:8787","agent_token":"bearer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := discoverOperatorHubURL()
	if err == nil || got != "" || !strings.Contains(err.Error(), "operator.json") {
		t.Fatalf("untrusted input discovery=%q err=%v", got, err)
	}
}

func TestOperatorHubDiscoveryRejectsMalformedConfigWithoutFallback(t *testing.T) {
	unsetEnvForTest(t, operatorHubURLEnv)
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `{"hub_url":`},
		{name: "missing", raw: `{}`},
		{name: "unknown", raw: `{"hub_url":"http://100.64.0.20:8787","agent_token":"secret"}`},
		{name: "duplicate", raw: `{"hub_url":"http://100.64.0.20:8787","hub_url":"http://100.64.0.21:8787"}`},
		{name: "trailing", raw: `{"hub_url":"http://100.64.0.20:8787"} {}`},
		{name: "wrong type", raw: `{"hub_url":7}`},
		{name: "bad endpoint", raw: `{"hub_url":"http://127.0.0.1:8787"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			operatorConfigFixture(t, test.raw, 0o600)
			got, err := discoverOperatorHubURL()
			if err == nil || got != "" {
				t.Fatalf("config discovery=%q err=%v", got, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("config error leaked ignored value: %v", err)
			}
		})
	}
}

func TestOperatorHubDiscoveryRejectsUnsafeConfigFile(t *testing.T) {
	unsetEnvForTest(t, operatorHubURLEnv)
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "symlink", prepare: func(t *testing.T, path string) {
			target := path + ".target"
			if err := os.WriteFile(target, []byte(`{"hub_url":"http://100.64.0.30:8787"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", prepare: func(t *testing.T, path string) {
			target := path + ".target"
			if err := os.WriteFile(target, []byte(`{"hub_url":"http://100.64.0.30:8787"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "group-writable", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"hub_url":"http://100.64.0.30:8787"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			// WriteFile modes are filtered by the process umask (022 strips the
			// group-write bit), so set the unsafe mode explicitly.
			if err := os.Chmod(path, 0o620); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			configHome := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", configHome)
			dir := filepath.Join(configHome, "clawctl")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, operatorConfigFilename)
			test.prepare(t, path)
			if got, err := discoverOperatorHubURL(); err == nil || got != "" {
				t.Fatalf("unsafe config discovery=%q err=%v", got, err)
			}
		})
	}
}

func TestOperatorHubDiscoveryRejectsWritableConfigParent(t *testing.T) {
	unsetEnvForTest(t, operatorHubURLEnv)
	path := operatorConfigFixture(t, `{"hub_url":"http://100.64.0.30:8787"}`, 0o600)
	if err := os.Chmod(filepath.Dir(path), 0o770); err != nil {
		t.Fatal(err)
	}
	if got, err := discoverOperatorHubURL(); err == nil || got != "" || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("writable parent discovery=%q err=%v", got, err)
	}
}

func managedHubStatusFixture(dbPath, fragment, binary, active, sub, pid string) string {
	return strings.Join([]string{
		"LoadState=loaded",
		"ActiveState=" + active,
		"SubState=" + sub,
		"MainPID=" + pid,
		"FragmentPath=" + fragment,
		"DropInPaths=",
		"NeedDaemonReload=no",
		"ExecStart={ path=" + binary + " ; argv[]=" + binary + " --db " + dbPath + " --listen ${CLAWCTL_LISTEN} --report-at ${CLAWCTL_REPORT_AT} --report-stamp ${CLAWCTL_REPORT_STAMP} ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }",
		"ExecCondition=",
		"ExecStartPre=",
		"ExecStartPost=",
		"ExecStop=",
		"ExecStopPost=",
		"Type=notify",
		"ExitType=main",
		"RemainAfterExit=no",
		"Restart=always",
		"KillMode=control-group",
		"Slice=app.slice",
		"Delegate=no",
		"ControlGroup=",
		"RootDirectory=",
		"RootImage=",
		"RootDirectoryStartOnly=no",
		"BindPaths=",
		"BindReadOnlyPaths=",
		"TemporaryFileSystem=",
		"ReadOnlyPaths=",
		"ReadWritePaths=" + filepath.Dir(dbPath),
		"InaccessiblePaths=",
		"ProtectHome=read-only",
		"ProtectSystem=strict",
		"PrivateTmp=yes",
		"PrivateMounts=no",
		"MountAPIVFS=no",
		"User=",
		"Group=",
		"SupplementaryGroups=",
		"DynamicUser=no",
		"RuntimeDirectory=",
		"StateDirectory=",
		"CacheDirectory=",
		"LogsDirectory=",
		"ConfigurationDirectory=",
		"MountImages=",
		"ExtensionImages=",
	}, "\n") + "\n"
}

func TestValidateStoppedHubUnitContractRequiresExactStoppedPinnedUnit(t *testing.T) {
	db := filepath.Join(t.TempDir(), "clawctl.sqlite")
	fragment := filepath.Join(t.TempDir(), managedHubUnit)
	binary := "/home/operator/.local/bin/clawctl-hub"
	base := managedHubStatusFixture(db, fragment, binary, "inactive", "dead", "0")
	if err := validateStoppedHubUnitContract(base, db, fragment, binary); err != nil {
		t.Fatalf("exact stopped contract: %v", err)
	}
	omittedEmptyArrays := base
	for _, property := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "MountImages", "ExtensionImages"} {
		omittedEmptyArrays = strings.Replace(omittedEmptyArrays, property+"=\n", "", 1)
	}
	if err := validateStoppedHubUnitContract(omittedEmptyArrays, db, fragment, binary); err != nil {
		t.Fatalf("systemctl-style omitted empty arrays rejected: %v", err)
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "active", raw: managedHubStatusFixture(db, fragment, binary, "active", "running", "42")},
		{name: "activating", raw: managedHubStatusFixture(db, fragment, binary, "activating", "start", "0")},
		{name: "failed", raw: managedHubStatusFixture(db, fragment, binary, "failed", "failed", "0")},
		{name: "pid remains", raw: managedHubStatusFixture(db, fragment, binary, "inactive", "dead", "42")},
		{name: "wrong db", raw: managedHubStatusFixture(db+".other", fragment, binary, "inactive", "dead", "0")},
		{name: "wrong fragment", raw: managedHubStatusFixture(db, fragment+".other", binary, "inactive", "dead", "0")},
		{name: "wrong binary", raw: managedHubStatusFixture(db, fragment, binary+".other", "inactive", "dead", "0")},
		{name: "second exec", raw: base + "ExecStart={ path=" + binary + " ; argv[]=" + binary + " --db " + db + " ; }\n"},
		{name: "drop in", raw: strings.Replace(base, "DropInPaths=", "DropInPaths=/tmp/override.conf", 1)},
		{name: "reload pending", raw: strings.Replace(base, "NeedDaemonReload=no", "NeedDaemonReload=yes", 1)},
		{name: "not loaded", raw: strings.Replace(base, "LoadState=loaded", "LoadState=not-found", 1)},
		{name: "exec condition", raw: strings.Replace(base, "ExecCondition=", "ExecCondition={ path=/tmp/check ; }", 1)},
		{name: "exec start pre", raw: strings.Replace(base, "ExecStartPre=", "ExecStartPre={ path=/tmp/pre ; }", 1)},
		{name: "exec start post", raw: strings.Replace(base, "ExecStartPost=", "ExecStartPost={ path=/tmp/post ; }", 1)},
		{name: "exec stop", raw: strings.Replace(base, "ExecStop=", "ExecStop={ path=/tmp/stop ; }", 1)},
		{name: "exec stop post", raw: strings.Replace(base, "ExecStopPost=", "ExecStopPost={ path=/tmp/post ; }", 1)},
		{name: "wrong type", raw: strings.Replace(base, "Type=notify", "Type=simple", 1)},
		{name: "wrong exit type", raw: strings.Replace(base, "ExitType=main", "ExitType=cgroup", 1)},
		{name: "remain after exit", raw: strings.Replace(base, "RemainAfterExit=no", "RemainAfterExit=yes", 1)},
		{name: "no restart", raw: strings.Replace(base, "Restart=always", "Restart=no", 1)},
		{name: "unsafe kill mode", raw: strings.Replace(base, "KillMode=control-group", "KillMode=process", 1)},
		{name: "wrong slice", raw: strings.Replace(base, "Slice=app.slice", "Slice=session.slice", 1)},
		{name: "delegated cgroup", raw: strings.Replace(base, "Delegate=no", "Delegate=yes", 1)},
		{name: "wrong cgroup", raw: strings.Replace(base, "ControlGroup=", "ControlGroup=/unexpected", 1)},
		{name: "root directory", raw: strings.Replace(base, "RootDirectory=", "RootDirectory=/tmp/root", 1)},
		{name: "bind path", raw: strings.Replace(base, "BindPaths=", "BindPaths=/tmp:/home/operator", 1)},
		{name: "wrong write path", raw: strings.Replace(base, "ReadWritePaths="+filepath.Dir(db), "ReadWritePaths=/tmp", 1)},
		{name: "private mounts", raw: strings.Replace(base, "PrivateMounts=no", "PrivateMounts=yes", 1)},
		{name: "dynamic user", raw: strings.Replace(base, "DynamicUser=no", "DynamicUser=yes", 1)},
		{name: "missing field", raw: strings.Replace(base, "MainPID=0\n", "", 1)},
		{name: "duplicate field", raw: "LoadState=loaded\n" + base},
		{name: "unknown field", raw: base + "Surprise=yes\n"},
		{name: "malformed field", raw: base + "broken\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateStoppedHubUnitContract(test.raw, db, fragment, binary); err == nil {
				t.Fatal("unsafe unit contract accepted")
			}
		})
	}
}

func TestExecStartRequiresExactManagedHubServeArgv(t *testing.T) {
	db := "/home/operator/.local/share/clawctl/clawctl.sqlite"
	binary := "/home/operator/.local/bin/clawctl-hub"
	wrap := func(args string) string {
		return "{ path=" + binary + " ; argv[]=" + binary + " " + args +
			" ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
	}
	exact := "--db " + db + " --listen ${CLAWCTL_LISTEN} --report-at ${CLAWCTL_REPORT_AT} --report-stamp ${CLAWCTL_REPORT_STAMP}"
	if err := execStartPinsManagedHub(wrap(exact), binary, db); err != nil {
		t.Fatalf("exact managed serve argv rejected: %v", err)
	}
	for _, args := range []string{
		"--db " + db,
		"--db=" + db + " --listen ${CLAWCTL_LISTEN} --report-at ${CLAWCTL_REPORT_AT} --report-stamp ${CLAWCTL_REPORT_STAMP}",
		"-db " + db + " --listen ${CLAWCTL_LISTEN} --report-at ${CLAWCTL_REPORT_AT} --report-stamp ${CLAWCTL_REPORT_STAMP}",
		exact + " --db /tmp/other.sqlite",
		"enroll-token " + exact + " victim",
		exact + " victim",
		exact + " --verbose",
		"--listen ${CLAWCTL_LISTEN} --db " + db + " --report-at ${CLAWCTL_REPORT_AT} --report-stamp ${CLAWCTL_REPORT_STAMP}",
		"--db " + db + " --listen 100.64.0.1:8787 --report-at ${CLAWCTL_REPORT_AT} --report-stamp ${CLAWCTL_REPORT_STAMP}",
		"--db " + db + " --listen ${CLAWCTL_LISTEN} --report-at /tmp/report --report-stamp ${CLAWCTL_REPORT_STAMP}",
		"--db " + db + " --listen ${CLAWCTL_LISTEN} --report-at ${CLAWCTL_REPORT_AT} --report-stamp /tmp/stamp",
	} {
		if err := execStartPinsManagedHub(wrap(args), binary, db); err == nil {
			t.Fatalf("non-canonical managed serve argv %q accepted", args)
		}
	}
}

func TestCurrentExecutableMustBeTheManagedBinaryInode(t *testing.T) {
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	running, err = filepath.EvalSymlinks(running)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCurrentExecutableMatches(running); err != nil {
		t.Fatalf("running executable rejected: %v", err)
	}
	copyPath := filepath.Join(t.TempDir(), "copied-test-binary")
	raw, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyCurrentExecutableMatches(copyPath); err == nil || !strings.Contains(err.Error(), "同一個 inode") {
		t.Fatalf("copied executable accepted: %v", err)
	}
}

func TestVerifyManagedHubStoppedPropagatesBoundedQueryFailure(t *testing.T) {
	db := filepath.Join(t.TempDir(), "clawctl.sqlite")
	want := context.DeadlineExceeded
	err := verifyManagedHubStoppedWithQuery(t.Context(), db, func(context.Context) ([]byte, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("query error=%v", err)
	}
	if err := verifyManagedHubStoppedWithQuery(nil, db, func(context.Context) ([]byte, error) {
		t.Fatal("nil context invoked systemd")
		return nil, nil
	}); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestVerifyManagedHubStoppedAcceptsOneExactSystemdSnapshot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	db := filepath.Join(t.TempDir(), "clawctl.sqlite")
	fragment, err := managedHubFragmentPath()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "systemd", "user", managedHubUnit); fragment != want {
		t.Fatalf("managed fragment=%q want fixed service path %q", fragment, want)
	}
	binary, err := managedHubBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	raw := managedHubStatusFixture(db, fragment, binary, "inactive", "dead", "0")
	if err := verifyManagedHubStoppedWithQuery(t.Context(), db, func(context.Context) ([]byte, error) {
		return []byte(raw), nil
	}); err != nil {
		t.Fatalf("exact stopped query: %v", err)
	}
}

func TestVerifyManagedHubStoppedRequiresEmptyExpectedCgroup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	db := filepath.Join(t.TempDir(), "clawctl.sqlite")
	fragment, err := managedHubFragmentPath()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := managedHubBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	wanted := "/user.slice/user-" + strconv.Itoa(os.Geteuid()) + ".slice/user@" +
		strconv.Itoa(os.Geteuid()) + ".service/app.slice/" + managedHubUnit
	raw := managedHubStatusFixture(db, fragment, binary, "inactive", "dead", "0")
	called := false
	wantErr := errors.New("cgroup still populated")
	err = verifyManagedHubStoppedWithChecks(t.Context(), db, func(context.Context) ([]byte, error) {
		return []byte(raw), nil
	}, func(got string) error {
		called = true
		if got != wanted {
			t.Fatalf("cgroup=%q want=%q", got, wanted)
		}
		return wantErr
	})
	if !called || !errors.Is(err, wantErr) {
		t.Fatalf("cgroup check called=%t err=%v", called, err)
	}
}

func TestVerifyManagedHubCgroupEmptyAtDistinguishesRemovedAndMalformedCgroups(t *testing.T) {
	root := t.TempDir()
	controlGroup := "/user.slice/example.service"
	cgroupPath := filepath.Join(root, "user.slice", "example.service")

	if err := verifyManagedHubCgroupEmptyAt(root, controlGroup); err != nil {
		t.Fatalf("removed service cgroup should prove empty: %v", err)
	}
	if err := os.MkdirAll(cgroupPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyManagedHubCgroupEmptyAt(root, controlGroup); err == nil || !strings.Contains(err.Error(), "缺少 cgroup.events") {
		t.Fatalf("existing cgroup without events accepted: %v", err)
	}

	eventsPath := filepath.Join(cgroupPath, "cgroup.events")
	for _, test := range []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "empty", raw: "populated 0\nfrozen 0\n", ok: true},
		{name: "populated", raw: "populated 1\nfrozen 0\n"},
		{name: "missing populated", raw: "frozen 0\n"},
		{name: "duplicate populated", raw: "populated 0\npopulated 0\n"},
		{name: "malformed", raw: "populated\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(eventsPath, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			err := verifyManagedHubCgroupEmptyAt(root, controlGroup)
			if test.ok && err != nil {
				t.Fatalf("valid empty cgroup: %v", err)
			}
			if !test.ok && err == nil {
				t.Fatal("unsafe cgroup.events accepted")
			}
		})
	}
}
