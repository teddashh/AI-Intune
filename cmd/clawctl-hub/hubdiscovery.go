package main

import (
	"bytes"
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

	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"github.com/teddashh/AI-Intune/internal/processenv"
	"golang.org/x/sys/unix"
)

const (
	operatorHubURLEnv      = "CLAWCTL_HUB_URL"
	operatorConfigFilename = "operator.json"
	maxOperatorConfigSize  = 16 << 10
	managedHubUnit         = "clawctl-hub.service"
	managedHubCheckTimeout = 5 * time.Second
	maxManagedHubStatus    = 64 << 10
	maxCgroupEventsSize    = 4 << 10
	managedCgroupRoot      = "/sys/fs/cgroup"
)

// discoverOperatorHubURL implements only the implicit portion of discovery.
// Explicit --hub-url and --db are resolved by machinecmd before this function
// is called. A selected source that is empty, unreadable or malformed is an
// error; it must never fall through to a lower-priority source or to SQLite.
//
// agent.json is deliberately not an input. It contains a machine bearer and
// is writable by the machine plane; that does not make it an operator trust
// anchor. hub.env and CLAWCTL_PUBLIC_URL are server configuration, not client
// discovery either.
func discoverOperatorHubURL() (string, error) {
	if raw, present := os.LookupEnv(operatorHubURLEnv); present {
		if raw == "" || strings.TrimSpace(raw) == "" {
			return "", fmt.Errorf("%s is set but empty", operatorHubURLEnv)
		}
		return validateDiscoveredOperatorURL(raw, operatorHubURLEnv)
	}

	path, err := operatorConfigPath()
	if err != nil {
		return "", fmt.Errorf("cannot find operator Hub URL; set %s or specify --hub-url: %w",
			operatorHubURLEnv, err)
	}
	raw, err := readOwnedRegularFile(path, maxOperatorConfigSize)
	if err != nil {
		return "", fmt.Errorf("failed to read operator discovery config %s; set %s or specify --hub-url: %w",
			path, operatorHubURLEnv, err)
	}
	hubURL, err := decodeOperatorConfig(raw)
	if err != nil {
		return "", fmt.Errorf("invalid operator discovery config %s: %w", path, err)
	}
	return validateDiscoveredOperatorURL(hubURL, path)
}

func validateDiscoveredOperatorURL(raw, source string) (string, error) {
	endpoint, err := operatorendpoint.ParseBaseURL(raw)
	if err != nil {
		// Do not echo raw: a malformed URL can contain userinfo even though this
		// contract rejects it, and discovery errors routinely reach shell logs.
		return "", fmt.Errorf("invalid operator Hub URL in %s: %w", source, err)
	}
	return endpoint.BaseURL(), nil
}

func operatorConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("get user config directory: %w", err)
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("user config directory is not an absolute path")
	}
	return filepath.Join(dir, "clawctl", operatorConfigFilename), nil
}

// decodeOperatorConfig accepts one exact schema. DisallowUnknownFields alone
// is insufficient because encoding/json permits duplicate object names; a
// duplicate hub_url would make two reviewers read different authorities.
func decodeOperatorConfig(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil {
		return "", errors.New("failed to parse JSON")
	}
	delim, ok := first.(json.Delim)
	if !ok || delim != '{' {
		return "", errors.New("top-level must be a JSON object")
	}
	seen := false
	var hubURL string
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return "", errors.New("failed to parse JSON object field")
		}
		name, ok := token.(string)
		if !ok {
			return "", errors.New("JSON object field name is not a string")
		}
		if name != "hub_url" {
			return "", fmt.Errorf("unknown field %q not allowed", name)
		}
		if seen {
			return "", errors.New("duplicate hub_url field")
		}
		seen = true
		if err := dec.Decode(&hubURL); err != nil {
			return "", errors.New("hub_url must be a string")
		}
	}
	last, err := dec.Token()
	if err != nil {
		return "", errors.New("JSON object did not terminate properly")
	}
	if delim, ok := last.(json.Delim); !ok || delim != '}' {
		return "", errors.New("JSON object did not terminate properly")
	}
	if token, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			_ = token
		}
		return "", errors.New("trailing value after JSON object")
	}
	if !seen {
		return "", errors.New("missing hub_url")
	}
	if hubURL == "" || strings.TrimSpace(hubURL) == "" {
		return "", errors.New("hub_url cannot be empty")
	}
	return hubURL, nil
}

// readOwnedRegularFile reads from the already-open descriptor and never
// follows the final path component. operator.json contains no bearer token,
// so 0644 is acceptable; another user must not own, write, replace through a
// hardlink, or redirect the selected file.
func readOwnedRegularFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("config path is not a canonical absolute path")
	}
	parentPath := filepath.Dir(path)
	resolvedParent, err := filepath.EvalSymlinks(parentPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config parent: %w", err)
	}
	parentFD, err := unix.Open(resolvedParent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)
	if err := validateDiscoveryParent(resolvedParent, parentFD); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(parentFD, filepath.Base(path),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("unable to create config file handle")
	}
	defer f.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.New("unable to verify config owner/link count")
	}
	if err := validateDiscoveryFile(&stat); err != nil {
		return nil, err
	}
	if stat.Size > limit {
		return nil, fmt.Errorf("config exceeds %d bytes", limit)
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("config exceeds %d bytes", limit)
	}
	var pathStat, afterStat unix.Stat_t
	if err := unix.Fstatat(parentFD, filepath.Base(path), &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("config path changed during read: %w", err)
	}
	if err := unix.Fstat(fd, &afterStat); err != nil {
		return nil, fmt.Errorf("re-stat config fd: %w", err)
	}
	if err := validateDiscoveryFile(&pathStat); err != nil {
		return nil, err
	}
	if err := validateDiscoveryFile(&afterStat); err != nil {
		return nil, err
	}
	if stat.Dev != pathStat.Dev || stat.Ino != pathStat.Ino ||
		stat.Dev != afterStat.Dev || stat.Ino != afterStat.Ino ||
		stat.Mode != afterStat.Mode || stat.Uid != afterStat.Uid || stat.Gid != afterStat.Gid ||
		stat.Nlink != afterStat.Nlink || stat.Size != afterStat.Size ||
		stat.Mtim != afterStat.Mtim || stat.Ctim != afterStat.Ctim {
		return nil, errors.New("config path/fd identity changed during read")
	}
	if err := validateDiscoveryParent(resolvedParent, parentFD); err != nil {
		return nil, err
	}
	return raw, nil
}

func validateDiscoveryParent(path string, fd int) error {
	var pathStat, fdStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil {
		return fmt.Errorf("lstat config parent: %w", err)
	}
	if err := unix.Fstat(fd, &fdStat); err != nil {
		return fmt.Errorf("fstat config parent: %w", err)
	}
	for _, stat := range []*unix.Stat_t{&pathStat, &fdStat} {
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) {
			return errors.New("config parent must be a directory owned by current user")
		}
		if stat.Mode&0o022 != 0 {
			return errors.New("config parent must not be writable by group/other")
		}
	}
	if pathStat.Dev != fdStat.Dev || pathStat.Ino != fdStat.Ino {
		return errors.New("config parent path/fd identity changed")
	}
	return nil
}

func validateDiscoveryFile(stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("config is not a regular file")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("config is not owned by current user")
	}
	if stat.Nlink != 1 {
		return errors.New("config link count is not 1")
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("config permissions are %04o; group/other must not be writable", stat.Mode&0o777)
	}
	return nil
}

type managedHubStatusQuery func(context.Context) ([]byte, error)
type managedHubCgroupCheck func(string) error

func verifyManagedHubStopped(ctx context.Context, dbPath string) error {
	binary, err := managedHubBinaryPath()
	if err != nil {
		return fmt.Errorf("unable to determine managed Hub binary path: %w", err)
	}
	if err := verifyCurrentExecutableMatches(binary); err != nil {
		return fmt.Errorf("direct DB CLI is not the same lock-capable binary started by managed Hub unit: %w", err)
	}
	return verifyManagedHubStoppedWithChecks(ctx, dbPath, queryManagedHubStatus, verifyManagedHubCgroupEmpty)
}

func queryManagedHubStatus(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	checkCtx, cancel := context.WithTimeout(ctx, managedHubCheckTimeout)
	defer cancel()
	cmd := processenv.CommandContext(checkCtx, "/usr/bin/systemctl", "--user", "show", managedHubUnit,
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID",
		"-p", "FragmentPath", "-p", "DropInPaths", "-p", "NeedDaemonReload", "-p", "ExecStart",
		"-p", "ExecCondition", "-p", "ExecStartPre", "-p", "ExecStartPost", "-p", "ExecStop", "-p", "ExecStopPost",
		"-p", "Type", "-p", "ExitType", "-p", "RemainAfterExit", "-p", "Restart",
		"-p", "KillMode", "-p", "Slice", "-p", "Delegate", "-p", "ControlGroup",
		"-p", "RootDirectory", "-p", "RootImage", "-p", "RootDirectoryStartOnly",
		"-p", "BindPaths", "-p", "BindReadOnlyPaths", "-p", "TemporaryFileSystem",
		"-p", "ReadOnlyPaths", "-p", "ReadWritePaths", "-p", "InaccessiblePaths",
		"-p", "ProtectHome", "-p", "ProtectSystem", "-p", "PrivateTmp", "-p", "PrivateMounts", "-p", "MountAPIVFS",
		"-p", "User", "-p", "Group", "-p", "SupplementaryGroups", "-p", "DynamicUser",
		"-p", "RuntimeDirectory", "-p", "StateDirectory", "-p", "CacheDirectory", "-p", "LogsDirectory", "-p", "ConfigurationDirectory",
		"-p", "MountImages", "-p", "ExtensionImages")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create %s status pipe: %w", managedHubUnit, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s status query: %w", managedHubUnit, err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(stdout, maxManagedHubStatus+1))
	if readErr != nil || len(raw) > maxManagedHubStatus {
		cancel()
		_ = cmd.Wait()
		if readErr != nil {
			return nil, fmt.Errorf("read %s status: %w", managedHubUnit, readErr)
		}
		return nil, fmt.Errorf("%s status exceeds %d bytes", managedHubUnit, maxManagedHubStatus)
	}
	err = cmd.Wait()
	if err != nil {
		if checkCtx.Err() != nil {
			return nil, fmt.Errorf("timeout verifying if %s is stopped: %w", managedHubUnit, checkCtx.Err())
		}
		return nil, fmt.Errorf("unable to read %s status: %w", managedHubUnit, err)
	}
	return raw, nil
}

func verifyManagedHubStoppedWithQuery(ctx context.Context, dbPath string, query managedHubStatusQuery) error {
	return verifyManagedHubStoppedWithChecks(ctx, dbPath, query, func(string) error { return nil })
}

func verifyManagedHubStoppedWithChecks(ctx context.Context, dbPath string, query managedHubStatusQuery, checkCgroup managedHubCgroupCheck) error {
	if ctx == nil {
		return errors.New("unable to verify Hub status: context is nil")
	}
	if query == nil {
		return errors.New("unable to verify Hub status: systemd query not initialized")
	}
	if checkCgroup == nil {
		return errors.New("unable to verify Hub status: cgroup checker not initialized")
	}
	raw, err := query(ctx)
	if err != nil {
		return err
	}
	fragment, err := managedHubFragmentPath()
	if err != nil {
		return fmt.Errorf("unable to determine managed Hub unit path: %w", err)
	}
	binary, err := managedHubBinaryPath()
	if err != nil {
		return fmt.Errorf("unable to determine managed Hub binary path: %w", err)
	}
	cgroup, err := stoppedHubUnitContract(string(raw), dbPath, fragment, binary)
	if err != nil {
		return err
	}
	if err := checkCgroup(cgroup); err != nil {
		return fmt.Errorf("%s cgroup is not empty: %w", managedHubUnit, err)
	}
	return nil
}

func managedHubFragmentPath() (string, error) {
	// The managed user unit and hub.env intentionally stay under %h/.config;
	// only the workstation-side operator.json follows XDG_CONFIG_HOME. Using
	// UserConfigDir here would let an ambient CLI variable move the stopped-unit
	// proof away from the path installed and loaded by our service lifecycle.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("user home is not an absolute path")
	}
	return filepath.Join(home, ".config", "systemd", "user", managedHubUnit), nil
}

func managedHubBinaryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("user home is not an absolute path")
	}
	return filepath.Join(home, ".local", "bin", "clawctl-hub"), nil
}

func validateStoppedHubUnitContract(raw, dbPath, expectedFragment, expectedBinary string) error {
	_, err := stoppedHubUnitContract(raw, dbPath, expectedFragment, expectedBinary)
	return err
}

// stoppedHubUnitContract returns the only cgroup pathname whose recursive
// population must be empty.  MainPID=0 by itself is insufficient: a drifted
// KillMode=process unit can be inactive while a child inherited the ledger.
func stoppedHubUnitContract(raw, dbPath, expectedFragment, expectedBinary string) (string, error) {
	wanted := []string{
		"LoadState", "ActiveState", "SubState", "MainPID", "FragmentPath", "DropInPaths", "NeedDaemonReload", "ExecStart",
		"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost",
		"Type", "ExitType", "RemainAfterExit", "Restart", "KillMode", "Slice", "Delegate", "ControlGroup",
		"RootDirectory", "RootImage", "RootDirectoryStartOnly", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem",
		"ReadOnlyPaths", "ReadWritePaths", "InaccessiblePaths", "ProtectHome", "ProtectSystem", "PrivateTmp", "PrivateMounts", "MountAPIVFS",
		"User", "Group", "SupplementaryGroups", "DynamicUser", "RuntimeDirectory", "StateDirectory", "CacheDirectory", "LogsDirectory",
		"ConfigurationDirectory", "MountImages", "ExtensionImages",
	}
	// systemctl omits unset command/image array properties on supported releases
	// even when explicitly selected with --property.
	optionalWhenEmpty := map[string]bool{
		"ExecCondition": true, "ExecStartPre": true, "ExecStartPost": true, "ExecStop": true, "ExecStopPost": true,
		"MountImages": true, "ExtensionImages": true,
	}
	properties := make(map[string]string, len(wanted))
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" {
			return "", errors.New("systemd Hub status format is incomplete")
		}
		recognized := false
		for _, allowed := range wanted {
			if name == allowed {
				recognized = true
				break
			}
		}
		if !recognized {
			return "", fmt.Errorf("systemd Hub status contains unknown field %q", name)
		}
		if _, duplicate := properties[name]; duplicate {
			return "", fmt.Errorf("duplicate field %s in systemd Hub status", name)
		}
		properties[name] = value
	}
	for _, name := range wanted {
		if optionalWhenEmpty[name] {
			continue
		}
		if _, ok := properties[name]; !ok {
			return "", fmt.Errorf("systemd Hub status missing %s", name)
		}
	}
	if properties["LoadState"] != "loaded" {
		return "", fmt.Errorf("%s LoadState=%s; managed unit verification failed", managedHubUnit, properties["LoadState"])
	}
	if properties["FragmentPath"] != expectedFragment {
		return "", fmt.Errorf("%s FragmentPath is not managed %s", managedHubUnit, expectedFragment)
	}
	if properties["DropInPaths"] != "" {
		return "", fmt.Errorf("%s has unproven drop-in", managedHubUnit)
	}
	if properties["NeedDaemonReload"] != "no" {
		return "", fmt.Errorf("%s NeedDaemonReload=%s; run systemctl --user daemon-reload", managedHubUnit, properties["NeedDaemonReload"])
	}
	for _, property := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost"} {
		if properties[property] != "" {
			return "", fmt.Errorf("%s has %s not included in stopped proof", managedHubUnit, property)
		}
	}
	for property, expected := range map[string]string{
		"Type": "notify", "ExitType": "main", "RemainAfterExit": "no", "Restart": "always",
		"KillMode": "control-group", "Slice": "app.slice", "Delegate": "no",
		"RootDirectory": "", "RootImage": "", "RootDirectoryStartOnly": "no",
		"BindPaths": "", "BindReadOnlyPaths": "", "TemporaryFileSystem": "",
		"ReadOnlyPaths": "", "InaccessiblePaths": "", "ProtectHome": "read-only", "ProtectSystem": "strict",
		"PrivateTmp": "yes", "PrivateMounts": "no", "MountAPIVFS": "no",
		"User": "", "Group": "", "SupplementaryGroups": "", "DynamicUser": "no",
		"RuntimeDirectory": "", "StateDirectory": "", "CacheDirectory": "", "LogsDirectory": "", "ConfigurationDirectory": "",
		"MountImages": "", "ExtensionImages": "",
	} {
		if properties[property] != expected {
			return "", fmt.Errorf("%s %s=%s; does not conform to managed stopped-service contract", managedHubUnit, property, properties[property])
		}
	}
	pid, err := strconv.ParseUint(properties["MainPID"], 10, 64)
	if err != nil || pid != 0 {
		return "", fmt.Errorf("%s MainPID=%s; writer is still running", managedHubUnit, properties["MainPID"])
	}
	if properties["ActiveState"] != "inactive" || properties["SubState"] != "dead" {
		return "", fmt.Errorf("%s is %s/%s; use systemctl --user stop %s first, then specify --db for break-glass",
			managedHubUnit, properties["ActiveState"], properties["SubState"], managedHubUnit)
	}
	if properties["ReadWritePaths"] != filepath.Dir(dbPath) {
		return "", fmt.Errorf("%s ReadWritePaths=%q; must uniquely pin DB parent %q so Hub and direct CLI see the same writer lock inode",
			managedHubUnit, properties["ReadWritePaths"], filepath.Dir(dbPath))
	}
	if err := execStartPinsManagedHub(properties["ExecStart"], expectedBinary, dbPath); err != nil {
		return "", fmt.Errorf("%s does not pin managed binary and the same direct DB: %w", managedHubUnit, err)
	}
	wantedCgroup := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice/%s", os.Geteuid(), os.Geteuid(), managedHubUnit)
	if properties["ControlGroup"] != "" && properties["ControlGroup"] != wantedCgroup {
		return "", fmt.Errorf("%s ControlGroup=%q; expected empty or %q", managedHubUnit, properties["ControlGroup"], wantedCgroup)
	}
	return wantedCgroup, nil
}

func execStartPinsManagedHub(execStart, expectedBinary, dbPath string) error {
	if dbPath == "" || !filepath.IsAbs(dbPath) || filepath.Clean(dbPath) != dbPath {
		return errors.New("DB path is not a canonical absolute path")
	}
	if expectedBinary == "" || !filepath.IsAbs(expectedBinary) || filepath.Clean(expectedBinary) != expectedBinary {
		return errors.New("managed binary path is not a canonical absolute path")
	}
	if strings.ContainsAny(dbPath+expectedBinary, " \t\r\n;\\\"'") {
		return errors.New("DB or managed binary path contains characters that systemd properties cannot unambiguously prove")
	}
	expectedPrefix := "{ path=" + expectedBinary + " ; argv[]="
	if !strings.HasPrefix(execStart, expectedPrefix) || strings.Count(execStart, "{ path=") != 1 {
		return fmt.Errorf("ExecStart path is not managed binary %q", expectedBinary)
	}
	const marker = "argv[]="
	start := strings.Index(execStart, marker)
	if start < 0 || strings.Index(execStart[start+len(marker):], marker) >= 0 {
		return errors.New("ExecStart does not have unique argv[]")
	}
	argvTail := execStart[start+len(marker):]
	end := strings.Index(argvTail, " ;")
	if end < 0 {
		return errors.New("ExecStart argv[] format is incomplete")
	}
	fields := strings.Fields(argvTail[:end])
	want := []string{
		expectedBinary,
		"--db", dbPath,
		"--listen", "${CLAWCTL_LISTEN}",
		"--report-at", "${CLAWCTL_REPORT_AT}",
		"--report-stamp", "${CLAWCTL_REPORT_STAMP}",
	}
	if len(fields) != len(want) {
		return errors.New("ExecStart argv is not a managed Hub serve command")
	}
	for i := range want {
		if fields[i] != want[i] {
			return fmt.Errorf("ExecStart argv[%d]=%q; expected %q", i, fields[i], want[i])
		}
	}
	return nil
}

func verifyCurrentExecutableMatches(expectedBinary string) error {
	if expectedBinary == "" || !filepath.IsAbs(expectedBinary) || filepath.Clean(expectedBinary) != expectedBinary {
		return errors.New("managed binary path is not a canonical absolute path")
	}
	var expected, running unix.Stat_t
	if err := unix.Lstat(expectedBinary, &expected); err != nil {
		return fmt.Errorf("lstat managed binary: %w", err)
	}
	if expected.Mode&unix.S_IFMT != unix.S_IFREG || expected.Uid != uint32(os.Geteuid()) || expected.Nlink != 1 {
		return errors.New("managed binary must be a regular file owned by current user with link count=1")
	}
	if err := unix.Stat("/proc/self/exe", &running); err != nil {
		return fmt.Errorf("stat running executable: %w", err)
	}
	if expected.Dev != running.Dev || expected.Ino != running.Ino {
		return errors.New("current CLI executable and managed Hub binary are not the same inode")
	}
	return nil
}

func verifyManagedHubCgroupEmpty(controlGroup string) error {
	var fs unix.Statfs_t
	if err := unix.Statfs(managedCgroupRoot, &fs); err != nil {
		return fmt.Errorf("unable to verify %s is unified cgroup v2: %w", managedCgroupRoot, err)
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("%s is not unified cgroup v2; cannot treat missing cgroup.events as unit drained", managedCgroupRoot)
	}
	return verifyManagedHubCgroupEmptyAt(managedCgroupRoot, controlGroup)
}

func verifyManagedHubCgroupEmptyAt(root, controlGroup string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("cgroup root is not a canonical absolute path")
	}
	if controlGroup == "" || !strings.HasPrefix(controlGroup, "/") || filepath.Clean(controlGroup) != controlGroup || controlGroup == "/" {
		return errors.New("ControlGroup is not a canonical absolute cgroup path")
	}
	cgroupPath := filepath.Join(root, strings.TrimPrefix(controlGroup, "/"))
	eventsPath := filepath.Join(cgroupPath, "cgroup.events")
	raw, err := os.ReadFile(eventsPath)
	if errors.Is(err, os.ErrNotExist) {
		// On unified cgroup v2 an existing cgroup always has cgroup.events.
		// systemd normally removes the service directory after a clean stop, so
		// distinguish that proof from an unsupported/malformed existing cgroup.
		info, statErr := os.Stat(cgroupPath)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("verify whether %s is removed: %w", cgroupPath, statErr)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s exists but is not a cgroup directory", cgroupPath)
		}
		return fmt.Errorf("%s still exists and is missing cgroup.events; unit process state unavailable", cgroupPath)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", eventsPath, err)
	}
	if len(raw) > maxCgroupEventsSize {
		return fmt.Errorf("%s exceeds %d bytes", eventsPath, maxCgroupEventsSize)
	}
	return validateCgroupEvents(eventsPath, raw)
}

func validateCgroupEvents(eventsPath string, raw []byte) error {
	seenPopulated := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok || name == "" || value == "" {
			return fmt.Errorf("%s format is incomplete", eventsPath)
		}
		if name != "populated" {
			continue
		}
		if seenPopulated {
			return fmt.Errorf("%s duplicate populated field", eventsPath)
		}
		seenPopulated = true
		if value != "0" {
			return fmt.Errorf("%s populated=%s, unit processes still present", eventsPath, value)
		}
	}
	if !seenPopulated {
		return fmt.Errorf("%s missing populated", eventsPath)
	}
	return nil
}
