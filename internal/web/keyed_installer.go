package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func (s *Server) downloadKeyedInstaller(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil || len(r.PostForm["token"]) != 1 || len(r.PostForm["arch"]) != 1 {
		http.Error(w, "Keyed installer unavailable", http.StatusNotFound)
		return
	}
	token, arch, machineID := r.PostForm.Get("token"), r.PostForm.Get("arch"), r.PathValue("id")
	name, _, err := s.store.PendingEnrollTicketMatches(machineID, token, time.Now())
	if err != nil {
		http.Error(w, "Keyed installer unavailable", http.StatusNotFound)
		return
	}
	if s.hubBase == "" {
		http.Error(w, "Configure the operator public URL before downloading a keyed installer", http.StatusConflict)
		return
	}
	if s.agentBundles == nil || (arch != "amd64" && arch != "arm64") {
		http.Error(w, "Keyed installer unavailable", http.StatusNotFound)
		return
	}
	spec, _ := lookupAgentBundleSpec(arch)
	want, ok := s.agentBundles.items[arch]
	if !ok {
		http.Error(w, "Keyed installer unavailable", http.StatusNotFound)
		return
	}
	file, got, err := inspectAgentBundle(s.agentBundles.dir, s.agentBundles.version, spec)
	if errors.Is(err, errAgentBundleMissing) {
		http.Error(w, "Keyed installer unavailable", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Agent bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	if got.SHA256 != want.SHA256 || got.SizeBytes != want.SizeBytes || got.Filename != want.Filename {
		http.Error(w, "Agent bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	// Snapshot and check the exact bytes we copy, including against in-place changes
	// after inspection. No secret archive is written to disk.
	source, err := io.ReadAll(io.LimitReader(file, maxAgentBundleBytes+1))
	digest := sha256.Sum256(source)
	if err != nil || int64(len(source)) != want.SizeBytes || hex.EncodeToString(digest[:]) != want.SHA256 {
		http.Error(w, "Agent bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := keyedAgentArchive(source, s.hubBase, token)
	if err != nil {
		http.Error(w, "Agent bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.operator.RecordKeyedInstallerDownload(operator.KeyedInstallerDownload{
		MachineID: machineID, DisplayName: name, Arch: arch,
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	}); err != nil {
		http.Error(w, "Keyed installer unavailable", http.StatusServiceUnavailable)
		return
	}
	filename := "clawctl-agent-" + keyedInstallerName(name) + "-linux-" + arch + ".tar.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	_, _ = w.Write(body)
}

func keyedInstallerName(name string) string {
	var out strings.Builder
	for _, c := range name {
		if out.Len() >= 80 {
			break
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			out.WriteRune(c)
		} else {
			out.WriteByte('-')
		}
	}
	if result := strings.Trim(out.String(), "-"); result != "" {
		return result
	}
	return "machine"
}

// source has already passed the generic archive validator and pinned checksum.
func keyedAgentArchive(source []byte, hub, token string) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var out bytes.Buffer
	zipper := gzip.NewWriter(&out)
	writer := tar.NewWriter(zipper)
	archive := tar.NewReader(reader)
	installerDir := ""
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if path.Base(header.Name) == "install-agent.sh" {
			installerDir = path.Dir(header.Name)
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := io.Copy(writer, archive); err != nil {
			return nil, err
		}
	}
	if installerDir == "" {
		return nil, errors.New("installer missing")
	}
	for _, file := range []struct {
		name, body string
		mode       int64
	}{
		{"hub-url", hub + "\n", 0o644}, {"enroll-token", token + "\n", 0o600},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: path.Join(installerDir, file.name), Typeflag: tar.TypeReg, Mode: file.mode, Size: int64(len(file.body))}); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(writer, file.body); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if err := zipper.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
