package main

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/model"
)

const defaultArtifactMaxBytes int64 = 1 << 30

var (
	artifactHTTPClient = &http.Client{Timeout: 30 * time.Minute}
	artifactNow        = time.Now
)

type artifactSidecar = artifact.Sidecar

type npmVersionMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
}

func artifactsDirFor(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "artifacts")
}

func cmdArtifact(argv []string) {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "用法：clawctl-hub artifact fetch|list|show [參數]")
		os.Exit(2)
	}
	switch argv[0] {
	case "fetch":
		cmdArtifactFetch(argv[1:])
	case "list", "show":
		if err := runArtifactReadCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
			!errors.Is(err, flag.ErrHelp) {
			log.Fatal(terminalSafe(err.Error()))
		}
	default:
		fmt.Fprintf(os.Stderr, "未知的子指令：artifact %s（有 fetch、list、show）\n", argv[0])
		os.Exit(2)
	}
}

func cmdArtifactFetch(argv []string) {
	if err := runArtifactFetchCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func fetchArtifact(ctx context.Context, artifactsDir, target, registry string, maxBytes int64,
	fetchedBy string) (artifactSidecar, bool, error) {
	name, version, err := parseArtifactTarget(target)
	if err != nil {
		return artifactSidecar{}, false, err
	}
	if maxBytes <= 0 || maxBytes == int64(^uint64(0)>>1) {
		return artifactSidecar{}, false, fmt.Errorf("--max-bytes 要大於 0，拿到 %d", maxBytes)
	}
	metadata, err := fetchNPMMetadata(ctx, registry, name, version)
	if err != nil {
		return artifactSidecar{}, false, err
	}
	if metadata.Version != version {
		return artifactSidecar{}, false, fmt.Errorf("registry 回的 version 是 %q，不是要求的 %q", metadata.Version, version)
	}
	if metadata.Name != "" && metadata.Name != name {
		return artifactSidecar{}, false, fmt.Errorf("registry 回的套件名是 %q，不是要求的 %q", metadata.Name, name)
	}
	if metadata.Dist.Tarball == "" || metadata.Dist.Integrity == "" {
		return artifactSidecar{}, false, errors.New("registry metadata 缺少 dist.tarball 或 dist.integrity")
	}
	expectedSHA512, err := decodeSHA512Integrity(metadata.Dist.Integrity)
	if err != nil {
		return artifactSidecar{}, false, fmt.Errorf("registry 的 dist.integrity 不合法：%w", err)
	}

	// 已收下的 sidecar 是 Hub 上一次親自量測的紀錄。metadata 的版本、URL 與
	// sha512 宣告都沒變，而且對應 tarball 的大小也還在，就不再抓一次。
	if cached, ok, err := findCachedArtifact(artifactsDir, name, version, metadata.Dist.Tarball,
		metadata.Dist.Integrity); err != nil {
		return artifactSidecar{}, false, fmt.Errorf("檢查既有 artifact：%w", err)
	} else if ok {
		return cached, true, nil
	}

	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return artifactSidecar{}, false, fmt.Errorf("建立 artifacts 目錄：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadata.Dist.Tarball, nil)
	if err != nil {
		return artifactSidecar{}, false, fmt.Errorf("建立 tarball request：%w", err)
	}
	resp, err := artifactHTTPClient.Do(req)
	if err != nil {
		return artifactSidecar{}, false, fmt.Errorf("下載 tarball：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return artifactSidecar{}, false, fmt.Errorf("下載 tarball 回了 HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(artifactsDir, ".artifact-*.tmp")
	if err != nil {
		return artifactSidecar{}, false, fmt.Errorf("建立 artifact 暫存檔：%w", err)
	}
	tmpPath := tmp.Name()
	keepTmp := false
	defer func() {
		_ = tmp.Close()
		if !keepTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	sha512Hash, sha256Hash := sha512.New(), sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, sha512Hash, sha256Hash),
		io.LimitReader(resp.Body, maxBytes+1))
	if copyErr != nil {
		return artifactSidecar{}, false, fmt.Errorf("下載 tarball：%w", copyErr)
	}
	if written > maxBytes {
		return artifactSidecar{}, false, fmt.Errorf("tarball 超過 --max-bytes=%d，已在 %d bytes 中止", maxBytes, written)
	}
	if subtle.ConstantTimeCompare(sha512Hash.Sum(nil), expectedSHA512) != 1 {
		// ⚠ 擋的是 registry 宣告的 sha512 與實際下載位元組不同；留下檔案會讓
		// 一個下載損壞或被動過的 tarball 冒充成 Hub 已量測並接受的 artifact。
		return artifactSidecar{}, false, fmt.Errorf("sha512 對不上 registry 的 dist.integrity；拒收下載的 tarball")
	}
	if err := tmp.Sync(); err != nil {
		return artifactSidecar{}, false, fmt.Errorf("同步 artifact 暫存檔：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return artifactSidecar{}, false, fmt.Errorf("關閉 artifact 暫存檔：%w", err)
	}
	sha256Hex := hex.EncodeToString(sha256Hash.Sum(nil))
	record := artifactSidecar{
		Name: name, Version: version, TarballURL: metadata.Dist.Tarball,
		SHA512Integrity: metadata.Dist.Integrity, SHA256: sha256Hex, Size: written,
		EnginesNode: metadata.Engines.Node, FetchedAt: artifactNow().UTC(), FetchedBy: fetchedBy,
	}

	destination := filepath.Join(artifactsDir, sha256Hex+".tgz")
	newTarball := false
	if info, statErr := os.Lstat(destination); statErr == nil {
		if !info.Mode().IsRegular() {
			return artifactSidecar{}, false, fmt.Errorf("artifact 目的地不是普通檔案：%s", destination)
		}
		if !artifact.FileHasSHA256(destination, sha256Hex) {
			// artifacts 是 Hub 自己的資料；同名檔若已經不再符合檔名所宣告的
			// sha256，就用這次通過上游 sha512 的完整下載原子替換。
			if err := os.Rename(tmpPath, destination); err != nil {
				return artifactSidecar{}, false, fmt.Errorf("替換損壞的 artifact：%w", err)
			}
			keepTmp = true
			newTarball = true
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return artifactSidecar{}, false, fmt.Errorf("檢查 artifact 目的地：%w", statErr)
	} else {
		if err := os.Rename(tmpPath, destination); err != nil {
			return artifactSidecar{}, false, fmt.Errorf("放入 artifact：%w", err)
		}
		keepTmp = true
		newTarball = true
	}
	if err := writeArtifactSidecar(artifactsDir, record); err != nil {
		if newTarball {
			_ = os.Remove(destination)
		}
		return artifactSidecar{}, false, err
	}
	return record, !newTarball, nil
}

func parseArtifactTarget(target string) (string, string, error) {
	name, version, ok := strings.Cut(target, "@")
	if !ok || name != "openclaw" || version == "" || strings.Contains(version, "@") {
		return "", "", fmt.Errorf("artifact 要寫成 openclaw@<version>，拿到 %q", target)
	}
	return name, version, nil
}

func fetchNPMMetadata(ctx context.Context, registry, name, version string) (npmVersionMetadata, error) {
	endpoint := strings.TrimRight(registry, "/") + "/" + url.PathEscape(name) + "/" + url.PathEscape(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return npmVersionMetadata{}, fmt.Errorf("建立 registry request：%w", err)
	}
	resp, err := artifactHTTPClient.Do(req)
	if err != nil {
		return npmVersionMetadata{}, fmt.Errorf("讀 registry metadata：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return npmVersionMetadata{}, fmt.Errorf("registry metadata 回了 HTTP %d", resp.StatusCode)
	}
	var metadata npmVersionMetadata
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err := dec.Decode(&metadata); err != nil {
		return npmVersionMetadata{}, fmt.Errorf("解析 registry metadata：%w", err)
	}
	return metadata, nil
}

func decodeSHA512Integrity(integrity string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok || encoded == "" {
		return nil, errors.New("只接受 sha512-<base64>")
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(b) != sha512.Size {
		return nil, errors.New("sha512 base64 解碼後不是 64 bytes")
	}
	return b, nil
}

func writeArtifactSidecar(dir string, record artifactSidecar) error {
	// 不做 HTML 轉義：sidecar 是人會 cat 來看的檔，engines 的 ">=22.19.0" 要是原文。
	b, err := marshalCompactNoEscape(record)
	if err != nil {
		return fmt.Errorf("編碼 artifact sidecar：%w", err)
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".sidecar-*.tmp")
	if err != nil {
		return fmt.Errorf("建立 sidecar 暫存檔：%w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return fmt.Errorf("寫入 artifact sidecar：%w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("同步 artifact sidecar：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("關閉 artifact sidecar：%w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, record.SHA256+".json")); err != nil {
		return fmt.Errorf("放入 artifact sidecar：%w", err)
	}
	keep = true
	return nil
}

func findCachedArtifact(dir, name, version, tarballURL, integrity string) (artifactSidecar, bool, error) {
	records, err := artifact.ReadSidecars(dir)
	if err != nil {
		return artifactSidecar{}, false, err
	}
	for _, record := range records {
		if record.Name != name || record.Version != version || record.TarballURL != tarballURL ||
			record.SHA512Integrity != integrity {
			continue
		}
		path := filepath.Join(dir, record.SHA256+".tgz")
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.Size() == record.Size && artifact.FileHasSHA256(path, record.SHA256) {
			return record, true, nil
		}
	}
	return artifactSidecar{}, false, nil
}

func (h *hub) handleGetArtifact(w http.ResponseWriter, r *http.Request, machineID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	digest := r.PathValue("sha256")
	if !artifact.ValidSHA256Hex(digest) {
		writeErr(w, http.StatusNotFound, model.ErrArtifactNotFound, "artifact 不存在")
		return
	}
	allowed, err := h.store.MachineMayDownloadArtifact(machineID, digest)
	if err != nil {
		log.Printf("核對 machine artifact grant 失敗 machine=%s artifact=%s: %v", machineID, digest, err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "核對 artifact download grant 失敗")
		return
	}
	if !allowed {
		// 不區分「bytes 不存在」與「這台沒有持有引用它的 active job」，避免
		// machine bearer token 把 content-addressed catalog 當列舉介面。
		writeErr(w, http.StatusNotFound, model.ErrArtifactNotFound, "artifact 不存在")
		return
	}
	// ⚠ 擋的是把 ../、編碼斜線或額外 path segment 變成任意檔案讀取：只有
	// 恰好 64 個小寫 hex 通過，路徑也只由 artifactsDir 與驗過的檔名組成。
	path := filepath.Join(h.artifactsDir, digest+".tgz")
	entry, err := os.Lstat(path)
	if err != nil || !entry.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, model.ErrArtifactNotFound, "artifact 不存在")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, model.ErrArtifactNotFound, "artifact 不存在")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(entry, info) {
		writeErr(w, http.StatusNotFound, model.ErrArtifactNotFound, "artifact 不存在")
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		log.Printf("串流 artifact %s 失敗：%v", digest, err)
	}
}
