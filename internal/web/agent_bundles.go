package web

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxAgentBundleBytes     int64 = 64 << 20
	maxAgentBundleFileBytes int64 = 64 << 20
)

type agentBundleView struct {
	OS        string
	Arch      string
	Filename  string
	URL       string
	Version   string
	SHA256    string
	SizeBytes int64
}

type agentInstallerView struct {
	OS      string
	Command string
}

type agentBundleCatalog struct {
	dir     string
	version string
	items   map[string]agentBundleView
	views   []agentBundleView
}

type agentBundleSpec struct {
	OS       string
	Arch     string
	Key      string
	Aliases  []string
	Filename string
}

type agentBundleArchiveEntry struct {
	name string
	mode int64
}

// Linux filenames stay on the original download keys so existing Hub links
// keep working. Darwin uses OS-qualified keys on the same {arch} route.
var linuxAgentBundleSpecs = []agentBundleSpec{
	{
		OS: "linux", Arch: "amd64", Key: "amd64", Aliases: []string{"linux-amd64"},
		Filename: "clawctl-agent-bootstrap-linux-amd64.tar.gz",
	},
	{
		OS: "linux", Arch: "arm64", Key: "arm64", Aliases: []string{"linux-arm64"},
		Filename: "clawctl-agent-bootstrap-linux-arm64.tar.gz",
	},
}

var darwinAgentBundleSpecs = []agentBundleSpec{
	{
		OS: "darwin", Arch: "amd64", Key: "darwin-amd64",
		Filename: "clawctl-agent-bootstrap-darwin-amd64.tar.gz",
	},
	{
		OS: "darwin", Arch: "arm64", Key: "darwin-arm64",
		Filename: "clawctl-agent-bootstrap-darwin-arm64.tar.gz",
	},
}

var linuxAgentBundleArchiveEntries = []agentBundleArchiveEntry{
	{name: "VERSION", mode: 0o644},
	{name: "clawctl-agent", mode: 0o755},
	{name: "clawctl-agent.service", mode: 0o644},
	{name: "clawctl-hermes.service", mode: 0o644},
	{name: "install-agent.sh", mode: 0o755},
	{name: "openclaw-gateway.service", mode: 0o644},
}

var darwinAgentBundleArchiveEntries = []agentBundleArchiveEntry{
	{name: "VERSION", mode: 0o644},
	{name: "clawctl-agent", mode: 0o755},
	{name: "clawctl-agent.plist", mode: 0o644},
	{name: "install-agent-macos.sh", mode: 0o755},
}

var windowsAgentBundleSpecs = []agentBundleSpec{
	{
		OS: "windows", Arch: "amd64", Key: "windows-amd64",
		Filename: "clawctl-agent-bootstrap-windows-amd64.tar.gz",
	},
	{
		OS: "windows", Arch: "arm64", Key: "windows-arm64",
		Filename: "clawctl-agent-bootstrap-windows-arm64.tar.gz",
	},
}

var windowsAgentBundleArchiveEntries = []agentBundleArchiveEntry{
	{name: "VERSION", mode: 0o644},
	{name: "clawctl-agent.exe", mode: 0o755},
	{name: "clawctl-agent.task.xml", mode: 0o644},
	{name: "install-agent-windows.ps1", mode: 0o755},
}

var errAgentBundleMissing = errors.New("bundle file is missing")

// Mach-O constants from Apple XNU:
// EXTERNAL_HEADERS/mach-o/loader.h (MH_MAGIC_64, MH_EXECUTE, mach_header_64)
// osfmk/mach/machine.h (CPU_TYPE_X86_64, CPU_TYPE_ARM64)
const (
	machoMagic64       = 0xfeedfacf
	machoFileExecute   = 2
	machoCPUTypeX86_64 = 0x01000007
	machoCPUTypeARM64  = 0x0100000c
	machoHeaderSize    = 32
)

// PE constants from Microsoft PE/COFF:
// IMAGE_DOS_SIGNATURE, IMAGE_NT_SIGNATURE, IMAGE_FILE_MACHINE_AMD64,
// IMAGE_FILE_MACHINE_ARM64, IMAGE_FILE_EXECUTABLE_IMAGE, IMAGE_FILE_DLL.
const (
	peDOSMagic            = 0x5A4D
	peNTSignature         = 0x00004550
	peMachineAMD64        = 0x8664
	peMachineARM64        = 0xaa64
	peFileExecutableImage = 0x0002
	peFileDLL             = 0x2000
	peMaxNTHeaderOffset   = 1024
)

// SetAgentBundles loads the immutable Linux amd64/arm64 bootstrap pair for the
// running Hub version. If any Darwin or Windows bundle is present in the same
// directory, that OS must also be a complete valid pair. The UI cannot offer a
// partially published OS.
func (s *Server) SetAgentBundles(dir, version string) error {
	if !validAgentBundleVersion(version) {
		return errors.New("agent bootstrap version is invalid")
	}
	catalog := &agentBundleCatalog{
		dir: dir, version: version, items: make(map[string]agentBundleView, 10),
	}
	for _, spec := range linuxAgentBundleSpecs {
		view, err := inspectAndCloseAgentBundle(dir, version, spec)
		if err != nil {
			return fmt.Errorf("agent bootstrap %s/%s: %w", spec.OS, spec.Arch, err)
		}
		indexAgentBundle(catalog, spec, view)
	}
	darwinViews, err := loadOptionalAgentBundlePair(catalog, dir, version, darwinAgentBundleSpecs)
	if err != nil {
		return err
	}
	windowsViews, err := loadOptionalAgentBundlePair(catalog, dir, version, windowsAgentBundleSpecs)
	if err != nil {
		return err
	}
	catalog.views = append(catalog.views, darwinViews...)
	catalog.views = append(catalog.views, windowsViews...)
	s.agentBundles = catalog
	return nil
}

func loadOptionalAgentBundlePair(catalog *agentBundleCatalog, dir, version string, specs []agentBundleSpec) ([]agentBundleView, error) {
	var views []agentBundleView
	for _, spec := range specs {
		view, err := inspectAndCloseAgentBundle(dir, version, spec)
		if errors.Is(err, errAgentBundleMissing) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("agent bootstrap %s/%s: %w", spec.OS, spec.Arch, err)
		}
		views = append(views, view)
		indexAgentBundle(catalog, spec, view)
	}
	if len(views) != 0 && len(views) != len(specs) {
		return nil, fmt.Errorf("%s agent bootstrap release is incomplete", specs[0].OS)
	}
	return views, nil
}

func inspectAndCloseAgentBundle(dir, version string, spec agentBundleSpec) (agentBundleView, error) {
	file, view, err := inspectAgentBundle(dir, version, spec)
	if file != nil {
		_ = file.Close()
	}
	return view, err
}

func indexAgentBundle(catalog *agentBundleCatalog, spec agentBundleSpec, view agentBundleView) {
	catalog.items[spec.Key] = view
	for _, alias := range spec.Aliases {
		catalog.items[alias] = view
	}
	if spec.OS == "linux" {
		catalog.views = append(catalog.views, view)
	}
}

func lookupAgentBundleSpec(key string) (agentBundleSpec, bool) {
	for _, spec := range linuxAgentBundleSpecs {
		if spec.Key == key {
			return spec, true
		}
		for _, alias := range spec.Aliases {
			if alias == key {
				return spec, true
			}
		}
	}
	for _, spec := range darwinAgentBundleSpecs {
		if spec.Key == key {
			return spec, true
		}
	}
	for _, spec := range windowsAgentBundleSpecs {
		if spec.Key == key {
			return spec, true
		}
	}
	return agentBundleSpec{}, false
}

func agentInstallerCommands(catalog *agentBundleCatalog, hubBase string) []agentInstallerView {
	hub := strings.TrimRight(hubBase, "/")
	if hub == "" {
		hub = "http://<Hub 的 literal Tailscale IP>:<CLAWCTL_LISTEN 的埠>"
	}
	out := []agentInstallerView{{
		OS: "linux", Command: "./install-agent.sh --hub " + hub,
	}}
	if catalog == nil {
		return out
	}
	for _, view := range catalog.views {
		if view.OS == "darwin" {
			out = append(out, agentInstallerView{
				OS: "darwin", Command: "./install-agent-macos.sh --hub " + hub,
			})
			break
		}
	}
	for _, view := range catalog.views {
		if view.OS == "windows" {
			out = append(out, agentInstallerView{
				OS: "windows", Command: `.\install-agent-windows.ps1 --hub ` + hub,
			})
			break
		}
	}
	return out
}

func validAgentBundleVersion(version string) bool {
	if version == "" || len(version) > 128 || version != strings.TrimSpace(version) {
		return false
	}
	for i, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || (i > 0 && strings.ContainsRune("._+-", r)) {
			continue
		}
		return false
	}
	return true
}

func (s *Server) downloadAgentBundle(w http.ResponseWriter, r *http.Request) {
	if s.agentBundles == nil {
		http.NotFound(w, r)
		return
	}
	key := r.PathValue("arch")
	want, ok := s.agentBundles.items[key]
	spec, specOK := lookupAgentBundleSpec(key)
	if !ok || !specOK {
		http.NotFound(w, r)
		return
	}
	file, got, err := inspectAgentBundle(s.agentBundles.dir, s.agentBundles.version, spec)
	if err != nil {
		http.Error(w, "Agent bootstrap 下載失敗", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	if got.SHA256 != want.SHA256 || got.SizeBytes != want.SizeBytes || got.Filename != want.Filename {
		http.Error(w, "Agent bootstrap 下載失敗", http.StatusServiceUnavailable)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "Agent bootstrap 下載失敗", http.StatusServiceUnavailable)
		return
	}
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "Agent bootstrap 下載失敗", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, want.Filename))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", fmt.Sprintf(`"sha256-%s"`, want.SHA256))
	http.ServeContent(w, r, want.Filename, info.ModTime(), file)
}

func inspectAgentBundle(dir, version string, spec agentBundleSpec) (*os.File, agentBundleView, error) {
	if spec.Filename == "" || spec.Key == "" {
		return nil, agentBundleView{}, errors.New("unsupported architecture")
	}
	file, err := openAgentBundleNoFollow(dir, spec.Filename)
	if err != nil {
		return nil, agentBundleView{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAgentBundleBytes {
		return nil, agentBundleView{}, errors.New("bundle is not a bounded regular file")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || stat.Nlink != 1 || stat.Uid != uint32(unix.Geteuid()) {
		return nil, agentBundleView{}, errors.New("bundle file ownership is invalid")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maxAgentBundleBytes+1))
	if err != nil || n != info.Size() || n > maxAgentBundleBytes {
		return nil, agentBundleView{}, errors.New("bundle bytes could not be read completely")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, agentBundleView{}, errors.New("bundle is not seekable")
	}
	if err := validateAgentBundleArchive(file, spec, version); err != nil {
		return nil, agentBundleView{}, err
	}
	after, err := file.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, agentBundleView{}, errors.New("bundle changed during inspection")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, agentBundleView{}, errors.New("bundle is not seekable")
	}
	keep = true
	return file, agentBundleView{
		OS: spec.OS, Arch: spec.Arch, Filename: spec.Filename,
		URL: "/downloads/agent/" + spec.Key, Version: version, SHA256: digest,
		SizeBytes: info.Size(),
	}, nil
}

func openAgentBundleNoFollow(dir, filename string) (*os.File, error) {
	if strings.TrimSpace(dir) == "" || filepath.Base(filename) != filename {
		return nil, errors.New("bundle directory is invalid")
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("bundle directory cannot be opened")
	}
	defer unix.Close(dirFD)
	var dirStat unix.Stat_t
	if err := unix.Fstat(dirFD, &dirStat); err != nil {
		return nil, errors.New("bundle directory cannot be inspected")
	}
	if dirStat.Uid != uint32(unix.Geteuid()) {
		return nil, errors.New("bundle directory owner is invalid")
	}
	if dirStat.Mode&0o022 != 0 {
		return nil, errors.New("bundle directory permissions are invalid")
	}
	fd, err := unix.Openat(dirFD, filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errAgentBundleMissing
		}
		return nil, errors.New("bundle file cannot be opened")
	}
	file := os.NewFile(uintptr(fd), filename)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("bundle descriptor cannot be opened")
	}
	return file, nil
}

func validateAgentBundleArchive(file *os.File, spec agentBundleSpec, version string) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return errors.New("bundle is not seekable")
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		return errors.New("bundle is not gzip")
	}
	gz.Multistream(false)
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	entries := linuxAgentBundleArchiveEntries
	switch spec.OS {
	case "darwin":
		entries = darwinAgentBundleArchiveEntries
	case "windows":
		entries = windowsAgentBundleArchiveEntries
	}
	var total int64
	for _, expected := range entries {
		header, err := tarReader.Next()
		if err != nil {
			return errors.New("bundle archive is incomplete")
		}
		if header.Name != expected.name || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) ||
			header.Mode&0o777 != expected.mode || header.Size <= 0 || header.Size > maxAgentBundleFileBytes {
			return errors.New("bundle archive entry is invalid")
		}
		total += header.Size
		if total > 2*maxAgentBundleFileBytes {
			return errors.New("bundle archive is too large")
		}
		switch expected.name {
		case "VERSION":
			versionBytes, err := io.ReadAll(tarReader)
			if err != nil || string(versionBytes) != version+"\n" {
				return errors.New("bundle version does not match Hub version")
			}
		case "clawctl-agent":
			if spec.OS == "darwin" {
				if err := validateAgentMachO(tarReader, header.Size, spec.Arch); err != nil {
					return err
				}
			} else if err := validateAgentELF(tarReader, header.Size, spec.Arch); err != nil {
				return err
			}
		case "clawctl-agent.exe":
			if err := validateAgentPE(tarReader, header.Size, spec.Arch); err != nil {
				return err
			}
		case "install-agent-macos.sh":
			if err := validateDarwinInstaller(tarReader, header.Size); err != nil {
				return err
			}
		case "clawctl-agent.plist":
			if err := validateDarwinPlist(tarReader, header.Size); err != nil {
				return err
			}
		case "install-agent-windows.ps1":
			if err := validateWindowsInstaller(tarReader, header.Size); err != nil {
				return err
			}
		case "clawctl-agent.task.xml":
			if err := validateWindowsTaskXML(tarReader, header.Size); err != nil {
				return err
			}
		default:
			if copied, err := io.Copy(io.Discard, tarReader); err != nil || copied != header.Size {
				return errors.New("bundle archive entry is truncated")
			}
		}
	}
	if header, err := tarReader.Next(); err != io.EOF || header != nil {
		return errors.New("bundle archive contains extra entries")
	}
	return nil
}

func validateAgentELF(reader io.Reader, size int64, arch string) error {
	var prefix [20]byte
	if size < int64(len(prefix)) {
		return errors.New("agent binary is truncated")
	}
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return errors.New("agent binary is truncated")
	}
	if string(prefix[0:4]) != "\x7fELF" || prefix[4] != 2 || prefix[5] != 1 {
		return errors.New("agent binary is not a 64-bit little-endian ELF")
	}
	wantMachine := uint16(62)
	if arch == "arm64" {
		wantMachine = 183
	}
	if binary.LittleEndian.Uint16(prefix[18:20]) != wantMachine {
		return errors.New("agent binary architecture does not match bundle")
	}
	if copied, err := io.Copy(io.Discard, reader); err != nil || copied != size-int64(len(prefix)) {
		return errors.New("agent binary is truncated")
	}
	return nil
}

func validateAgentMachO(reader io.Reader, size int64, arch string) error {
	if size < machoHeaderSize {
		return errors.New("agent binary is truncated")
	}
	var prefix [16]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return errors.New("agent binary is truncated")
	}
	if binary.LittleEndian.Uint32(prefix[0:4]) != machoMagic64 {
		return errors.New("agent binary is not a 64-bit Mach-O executable")
	}
	wantCPU := uint32(machoCPUTypeX86_64)
	if arch == "arm64" {
		wantCPU = machoCPUTypeARM64
	}
	if binary.LittleEndian.Uint32(prefix[4:8]) != wantCPU {
		return errors.New("agent binary architecture does not match bundle")
	}
	if binary.LittleEndian.Uint32(prefix[12:16]) != machoFileExecute {
		return errors.New("agent binary is not a 64-bit Mach-O executable")
	}
	if copied, err := io.Copy(io.Discard, reader); err != nil || copied != size-int64(len(prefix)) {
		return errors.New("agent binary is truncated")
	}
	return nil
}

func validateDarwinInstaller(reader io.Reader, size int64) error {
	body, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil || int64(len(body)) != size {
		return errors.New("bundle archive entry is truncated")
	}
	text := string(body)
	if !strings.HasPrefix(text, "#!") || !strings.Contains(text, "Darwin") ||
		!strings.Contains(text, "launchctl") || !strings.Contains(text, "clawctl-agent.plist") ||
		!strings.Contains(text, "--hub") || !strings.Contains(text, "com.clawctl.agent") {
		return errors.New("bundle installer is not the macOS agent installer")
	}
	return nil
}

func validateDarwinPlist(reader io.Reader, size int64) error {
	body, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil || int64(len(body)) != size {
		return errors.New("bundle archive entry is truncated")
	}
	text := string(body)
	if !strings.Contains(text, "<?xml") || !strings.Contains(text, "<plist") ||
		!strings.Contains(text, "<key>Label</key>") ||
		!strings.Contains(text, "<string>com.clawctl.agent</string>") ||
		!strings.Contains(text, "@@CLAWCTL_AGENT_BIN@@") ||
		!strings.Contains(text, "@@CLAWCTL_AGENT_LOG@@") ||
		!strings.Contains(text, "<key>KeepAlive</key>") ||
		strings.Contains(text, "[Unit]") {
		return errors.New("bundle plist is not the macOS LaunchAgent template")
	}
	return nil
}

func validateAgentPE(reader io.Reader, size int64, arch string) error {
	if size < 64 {
		return errors.New("agent binary is truncated")
	}
	prefixLen := int(size)
	if prefixLen > peMaxNTHeaderOffset+24 {
		prefixLen = peMaxNTHeaderOffset + 24
	}
	prefix := make([]byte, prefixLen)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		return errors.New("agent binary is truncated")
	}
	if binary.LittleEndian.Uint16(prefix[0:2]) != peDOSMagic {
		return errors.New("agent binary is not a Windows PE executable")
	}
	ntOffset := int(binary.LittleEndian.Uint32(prefix[0x3C:0x40]))
	if ntOffset < 64 || ntOffset > peMaxNTHeaderOffset || ntOffset+24 > len(prefix) || int64(ntOffset)+24 > size {
		return errors.New("agent binary is not a Windows PE executable")
	}
	if binary.LittleEndian.Uint32(prefix[ntOffset:ntOffset+4]) != peNTSignature {
		return errors.New("agent binary is not a Windows PE executable")
	}
	wantMachine := uint16(peMachineAMD64)
	if arch == "arm64" {
		wantMachine = peMachineARM64
	}
	if binary.LittleEndian.Uint16(prefix[ntOffset+4:ntOffset+6]) != wantMachine {
		return errors.New("agent binary architecture does not match bundle")
	}
	characteristics := binary.LittleEndian.Uint16(prefix[ntOffset+22 : ntOffset+24])
	if characteristics&peFileExecutableImage == 0 || characteristics&peFileDLL != 0 {
		return errors.New("agent binary is not a Windows PE executable")
	}
	if copied, err := io.Copy(io.Discard, reader); err != nil || copied != size-int64(len(prefix)) {
		return errors.New("agent binary is truncated")
	}
	return nil
}

func validateWindowsInstaller(reader io.Reader, size int64) error {
	body, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil || int64(len(body)) != size {
		return errors.New("bundle archive entry is truncated")
	}
	text := string(body)
	if !strings.Contains(text, "Windows") ||
		!strings.Contains(text, "Register-ScheduledTask") ||
		!strings.Contains(text, "clawctl-agent.task.xml") ||
		!strings.Contains(text, "--hub") ||
		!strings.Contains(text, "clawctl-agent") ||
		!strings.Contains(text, "--require-platform-evidence") ||
		strings.Contains(text, "launchctl") {
		return errors.New("bundle installer is not the Windows agent installer")
	}
	return nil
}

func validateWindowsTaskXML(reader io.Reader, size int64) error {
	body, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil || int64(len(body)) != size {
		return errors.New("bundle archive entry is truncated")
	}
	text := string(body)
	if !strings.Contains(text, "<?xml") || !strings.Contains(text, "<Task") ||
		!strings.Contains(text, "<LogonTrigger>") ||
		!strings.Contains(text, "InteractiveToken") ||
		!strings.Contains(text, "LeastPrivilege") ||
		!strings.Contains(text, "@@CLAWCTL_AGENT_BIN@@") ||
		!strings.Contains(text, `\clawctl\clawctl-agent`) ||
		strings.Contains(text, "[Unit]") || strings.Contains(text, "<plist") {
		return errors.New("bundle task XML is not the Windows scheduled-task template")
	}
	return nil
}
