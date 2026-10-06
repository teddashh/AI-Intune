package probe

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// versionCacheMaxAge is a safety bound for a successful --version result.
// The identity key already misses a replaced binary; this catches a result
// that depended on something the key does not name (for example a nested
// interpreter, or a shebang line this process could not read).
const versionCacheMaxAge = 24 * time.Hour

// versionProbeCache remembers successful `<tool> --version` output for the
// life of this process.
//
// The key is the resolved executable (path after symlinks, device, inode,
// size, mtime) plus the PATH string the child will see. A `#!` script also
// contributes the identity of the interpreter it names, when that is cheap:
// one short read and, for `#!/usr/bin/env name`, a PATH lookup plus a stat.
//
// Limitations, on purpose:
//   - Only one interpreter level. A wrapper's `#!/usr/bin/env node` records
//     that node binary. If node is itself a script, the script's own
//     interpreter (often /bin/sh) is not part of the key. Replacing that
//     outer interpreter does not force a re-exec until the 24h bound, unless
//     PATH or the recorded files change.
//   - Shebang lines longer than 255 bytes without a newline are not parsed.
//     `env -Sname` (the flag glued to the name) is not parsed. In both cases
//     the PATH string is still part of the key, so a PATH change re-execs.
//   - On Windows the portable stat has no device/inode. Identity is the
//     resolved path, size, and mtime. A same-size replacement that keeps
//     mtime would wait for the 24h bound. The same wait applies if a
//     filesystem reuses the device and inode and the mtime does not change.
//   - Failures are not stored. A timeout or a missing node is retried on the
//     next probe, which for Collect is the next observation cycle. A failure
//     is therefore never reused, which is stricter than caching it for one cycle.
//   - The cache stores the trimmed stdout/stderr of a zero-exit probe with
//     non-empty stdout. Observation fields are still derived the same way.
type versionCacheKey struct {
	file       fileIdentity
	pathEnv    string
	interp     fileIdentity
	interpMiss string
}

type fileIdentity struct {
	path  string
	dev   uint64
	ino   uint64
	size  int64
	mtime int64
}

type versionCacheEntry struct {
	stdout string
	stderr string
	stored time.Time
}

var versionProbeCache = struct {
	sync.Mutex
	entries map[versionCacheKey]versionCacheEntry
}{entries: make(map[versionCacheKey]versionCacheEntry)}

func versionProbeKey(name string, dirs []string, args []string) (versionCacheKey, bool) {
	if len(args) != 1 || args[0] != "--version" {
		return versionCacheKey{}, false
	}
	id, ok := fileIdentityOf(name)
	if !ok {
		return versionCacheKey{}, false
	}
	key := versionCacheKey{file: id, pathEnv: childPATHKey(dirs)}
	shebang := parseShebang(id.path, childPATHDirs(dirs))
	switch {
	case shebang.path != "":
		if iid, ok := fileIdentityOf(shebang.path); ok {
			key.interp = iid
		} else {
			key.interpMiss = shebang.path
		}
	case shebang.missing != "":
		key.interpMiss = shebang.missing
	}
	return key, true
}

func versionProbeLoad(key versionCacheKey) (string, string, bool) {
	versionProbeCache.Lock()
	defer versionProbeCache.Unlock()
	entry, ok := versionProbeCache.entries[key]
	if !ok {
		return "", "", false
	}
	if entry.stored.IsZero() || time.Since(entry.stored) >= versionCacheMaxAge {
		delete(versionProbeCache.entries, key)
		return "", "", false
	}
	return entry.stdout, entry.stderr, true
}

func versionProbeStore(key versionCacheKey, stdout, stderr string) {
	versionProbeCache.Lock()
	defer versionProbeCache.Unlock()
	versionProbeCache.entries[key] = versionCacheEntry{
		stdout: stdout,
		stderr: stderr,
		stored: time.Now(),
	}
}

func fileIdentityOf(path string) (fileIdentity, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fileIdentity{}, false
	}
	resolved = filepath.Clean(resolved)
	fi, err := os.Stat(resolved)
	if err != nil {
		return fileIdentity{}, false
	}
	id := fileIdentity{
		path:  resolved,
		size:  fi.Size(),
		mtime: fi.ModTime().UnixNano(),
	}
	if dev, ino, ok := deviceInode(fi); ok {
		id.dev = dev
		id.ino = ino
	}
	return id, true
}

func childPATHDirs(dirs []string) []string {
	if len(dirs) == 0 {
		return filepath.SplitList(os.Getenv("PATH"))
	}
	return dirs
}

func childPATHKey(dirs []string) string {
	if len(dirs) == 0 {
		return os.Getenv("PATH")
	}
	return strings.Join(dirs, string(filepath.ListSeparator))
}

type shebangInterp struct {
	path    string
	missing string
}

func parseShebang(resolvedPath string, dirs []string) shebangInterp {
	line, ok := readShebangLine(resolvedPath)
	if !ok {
		return shebangInterp{}
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return shebangInterp{}
	}
	if filepath.Base(fields[0]) == "env" {
		name, ok := envInterpreterName(fields[1:])
		if !ok {
			return shebangInterp{missing: "env"}
		}
		if strings.ContainsRune(name, os.PathSeparator) {
			if filepath.IsAbs(name) {
				return shebangInterp{path: name}
			}
			return shebangInterp{missing: name}
		}
		found, ok := lookPathIn(dirs, name)
		if !ok {
			return shebangInterp{missing: name}
		}
		return shebangInterp{path: found}
	}
	if filepath.IsAbs(fields[0]) {
		return shebangInterp{path: fields[0]}
	}
	return shebangInterp{missing: fields[0]}
}

// envInterpreterName returns the program name from `env` arguments.
// Flags are skipped. `env -S name` is recognized. `env -Sname` is not.
func envInterpreterName(args []string) (string, bool) {
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		if arg == "-S" {
			i++
			break
		}
		if strings.HasPrefix(arg, "-S") && len(arg) > 2 {
			return "", false
		}
		i++
	}
	if i >= len(args) {
		return "", false
	}
	return args[i], true
}

// readShebangLine returns the interpreter text after #!, without the newline.
// A binary, a read error, or an overlong first line yields false.
func readShebangLine(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, err := f.Read(buf)
	if n < 2 || (err != nil && err != io.EOF) {
		return "", false
	}
	buf = buf[:n]
	nl := bytes.IndexByte(buf, '\n')
	if nl < 0 {
		if n == len(buf) && n == 256 {
			return "", false
		}
		nl = n
	}
	line := strings.TrimRight(string(buf[:nl]), "\r")
	if !strings.HasPrefix(line, "#!") {
		return "", false
	}
	return strings.TrimSpace(line[2:]), true
}
