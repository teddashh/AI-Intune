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
	Arch      string
	Filename  string
	URL       string
	Version   string
	SHA256    string
	SizeBytes int64
}

type agentBundleCatalog struct {
	dir     string
	version string
	items   map[string]agentBundleView
	views   []agentBundleView
}

var agentBundleFilenames = map[string]string{
	"amd64": "clawctl-agent-bootstrap-linux-amd64.tar.gz",
	"arm64": "clawctl-agent-bootstrap-linux-arm64.tar.gz",
}

type agentBundleArchiveEntry struct {
	name string
	mode int64
}

var agentBundleArchiveEntries = []agentBundleArchiveEntry{
	{name: "VERSION", mode: 0o644},
	{name: "clawctl-agent", mode: 0o755},
	{name: "clawctl-agent.service", mode: 0o644},
	{name: "clawctl-hermes.service", mode: 0o644},
	{name: "install-agent.sh", mode: 0o755},
	{name: "openclaw-gateway.service", mode: 0o644},
}

// SetAgentBundles loads the two immutable bootstrap bundles published for the
// running Hub version. Both architectures are admitted as one release: the UI
// cannot offer a partially published version.
func (s *Server) SetAgentBundles(dir, version string) error {
	if !validAgentBundleVersion(version) {
		return errors.New("agent bootstrap version is invalid")
	}
	catalog := &agentBundleCatalog{
		dir: dir, version: version, items: make(map[string]agentBundleView, len(agentBundleFilenames)),
	}
	for _, arch := range []string{"amd64", "arm64"} {
		file, view, err := inspectAgentBundle(dir, version, arch)
		if file != nil {
			_ = file.Close()
		}
		if err != nil {
			return fmt.Errorf("agent bootstrap %s: %w", arch, err)
		}
		catalog.items[arch] = view
		catalog.views = append(catalog.views, view)
	}
	s.agentBundles = catalog
	return nil
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
	arch := r.PathValue("arch")
	want, ok := s.agentBundles.items[arch]
	if !ok {
		http.NotFound(w, r)
		return
	}
	file, got, err := inspectAgentBundle(s.agentBundles.dir, s.agentBundles.version, arch)
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

func inspectAgentBundle(dir, version, arch string) (*os.File, agentBundleView, error) {
	filename, ok := agentBundleFilenames[arch]
	if !ok {
		return nil, agentBundleView{}, errors.New("unsupported architecture")
	}
	file, err := openAgentBundleNoFollow(dir, filename)
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
	if err := validateAgentBundleArchive(file, arch, version); err != nil {
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
		Arch: arch, Filename: filename, URL: "/downloads/agent/" + arch,
		Version: version, SHA256: digest, SizeBytes: info.Size(),
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
		return nil, errors.New("bundle file cannot be opened")
	}
	file := os.NewFile(uintptr(fd), filename)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("bundle descriptor cannot be opened")
	}
	return file, nil
}

func validateAgentBundleArchive(file *os.File, arch, version string) error {
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
	var total int64
	for _, expected := range agentBundleArchiveEntries {
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
		if expected.name == "VERSION" {
			versionBytes, err := io.ReadAll(tarReader)
			if err != nil || string(versionBytes) != version+"\n" {
				return errors.New("bundle version does not match Hub version")
			}
		} else if expected.name == "clawctl-agent" {
			if err := validateAgentELF(tarReader, header.Size, arch); err != nil {
				return err
			}
		} else if copied, err := io.Copy(io.Discard, tarReader); err != nil || copied != header.Size {
			return errors.New("bundle archive entry is truncated")
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
