package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"golang.org/x/sys/unix"
)

const (
	artifactFetchRecoverySchemaVersion = 1
	artifactFetchRecoveryMaxBytes      = 8 << 10
)

// artifactFetchRecovery is the private receipt that bridges the only unsafe
// uncertainty window: the process may lose the HTTP response after the Hub
// accepted create. It contains the full canonical request but is never sent
// to terminal output. The Hub's durable idempotency receipt remains authority.
type artifactFetchRecovery struct {
	SchemaVersion  int    `json:"schema_version"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	ConfirmVersion string `json:"confirm_version"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
	PreviewDigest  string `json:"preview_digest"`
	HubURL         string `json:"hub_url"`
	RequestDigest  string `json:"request_digest"`
}

func artifactFetchRecoveryFromInput(input artifactFetchCLIInput, hubURL string) artifactFetchRecovery {
	document := artifactFetchRecovery{
		SchemaVersion: artifactFetchRecoverySchemaVersion,
		Name:          input.name, Version: input.version, ConfirmVersion: input.confirmVersion,
		Reason: input.reason, IdempotencyKey: input.idempotencyKey,
		PreviewDigest: input.previewDigest, HubURL: hubURL,
	}
	document.RequestDigest = artifactFetchRecoveryRequestDigest(document)
	return document
}

func artifactFetchRecoveryRequestDigest(document artifactFetchRecovery) string {
	document.RequestDigest = ""
	raw, err := json.Marshal(document)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateArtifactFetchRecovery(document artifactFetchRecovery) error {
	name, version, targetErr := parseArtifactFetchCLITarget(document.Name + "@" + document.Version)
	if document.SchemaVersion != artifactFetchRecoverySchemaVersion || targetErr != nil ||
		name != document.Name || version != document.Version || document.ConfirmVersion != document.Version {
		return errors.New("artifact fetch recovery file target/confirmation 不合法")
	}
	if err := validateArtifactFetchCLIValue("reason", document.Reason, 500, false); err != nil {
		return errors.New("artifact fetch recovery file reason 不合法")
	}
	if err := validateArtifactFetchCLIValue("idempotency-key", document.IdempotencyKey, 200, false); err != nil ||
		!validDeploymentPreviewDigest(document.PreviewDigest) {
		return errors.New("artifact fetch recovery file replay coordinates 不合法")
	}
	endpoint, err := operatorendpoint.ParseBaseURL(document.HubURL)
	if err != nil || endpoint.BaseURL() != document.HubURL {
		return errors.New("artifact fetch recovery file HTTP authority 不合法")
	}
	if document.RequestDigest == "" || document.RequestDigest != artifactFetchRecoveryRequestDigest(document) {
		return errors.New("artifact fetch recovery file request digest 不符")
	}
	return nil
}

func marshalArtifactFetchRecovery(document artifactFetchRecovery) ([]byte, error) {
	if err := validateArtifactFetchRecovery(document); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, errors.New("artifact fetch recovery file 無法編碼")
	}
	raw = append(raw, '\n')
	if len(raw) > artifactFetchRecoveryMaxBytes {
		return nil, errors.New("artifact fetch recovery file 過大")
	}
	return raw, nil
}

func decodeArtifactFetchRecovery(raw []byte) (artifactFetchRecovery, error) {
	if len(raw) == 0 || len(raw) > artifactFetchRecoveryMaxBytes {
		return artifactFetchRecovery{}, errors.New("artifact fetch recovery file 大小不合法")
	}
	var document artifactFetchRecovery
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return artifactFetchRecovery{}, errors.New("artifact fetch recovery file JSON 不合法")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return artifactFetchRecovery{}, errors.New("artifact fetch recovery file 含 trailing JSON")
	}
	canonical, err := marshalArtifactFetchRecovery(document)
	if err != nil || !bytes.Equal(raw, canonical) {
		return artifactFetchRecovery{}, errors.New("artifact fetch recovery file 不是 canonical JSON")
	}
	return document, nil
}

func defaultArtifactFetchRecoveryPath(key string) (string, error) {
	// Reuse the hardened per-user 0700 recovery directory. The action prefix
	// separates this namespace from every deployment receipt.
	return defaultDeploymentRecoveryPath("artifact-fetch", key)
}

func loadArtifactFetchRecoveryIfExists(path string) (artifactFetchRecovery, bool, error) {
	if !validDeploymentRecoveryAbsolutePath(path) {
		return artifactFetchRecovery{}, false, errors.New("artifact fetch --recovery-file 必須是 canonical absolute path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return artifactFetchRecovery{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return artifactFetchRecovery{}, false, errors.New("artifact fetch recovery file 必須是既有 non-symlink regular file")
	}
	raw, err := readPrivateDeploymentRecovery(path)
	if err != nil {
		return artifactFetchRecovery{}, false, errors.New("artifact fetch recovery file 必須是目前使用者持有的 0600 regular file")
	}
	document, err := decodeArtifactFetchRecovery(raw)
	if err != nil {
		return artifactFetchRecovery{}, false, err
	}
	return document, true, nil
}

func ensureArtifactFetchRecovery(path string, document artifactFetchRecovery, loaded bool) error {
	if loaded {
		return durablyVerifyArtifactFetchRecovery(path, document)
	}
	existing, exists, err := loadArtifactFetchRecoveryIfExists(path)
	if err != nil {
		return err
	}
	if exists {
		if reflect.DeepEqual(existing, document) {
			return nil
		}
		return errors.New("artifact fetch recovery file 已存在且屬於不同 request；拒絕覆寫")
	}
	return writeArtifactFetchRecoveryNoClobber(path, document)
}

func writeArtifactFetchRecoveryNoClobber(path string, document artifactFetchRecovery) error {
	if !validDeploymentRecoveryAbsolutePath(path) {
		return errors.New("artifact fetch --recovery-file 必須是 canonical absolute path")
	}
	raw, err := marshalArtifactFetchRecovery(document)
	if err != nil {
		return err
	}
	resolvedParent, parentFD, err := openDeploymentRecoveryParent(path, true)
	if err != nil {
		return errors.New("artifact fetch recovery parent 無法建立或安全開啟")
	}
	defer unix.Close(parentFD)
	tmpName, fd, err := createDeploymentRecoveryTemp(parentFD)
	if err != nil {
		return errors.New("artifact fetch recovery temp file 無法建立")
	}
	tmpExists := true
	defer func() {
		if tmpExists {
			_ = unix.Unlinkat(parentFD, tmpName, 0)
		}
	}()
	file := os.NewFile(uintptr(fd), "artifact-fetch-recovery-temp")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("artifact fetch recovery temp handle 無法建立")
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return errors.New("artifact fetch recovery file 無法寫入")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("artifact fetch recovery file 無法 sync")
	}
	if err := file.Close(); err != nil {
		return errors.New("artifact fetch recovery file 無法關閉")
	}
	if err := unix.Linkat(parentFD, tmpName, parentFD, filepath.Base(path), 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errors.New("artifact fetch recovery file 已存在；拒絕覆寫")
		}
		return errors.New("artifact fetch recovery file 無法原子發布")
	}
	if err := unix.Unlinkat(parentFD, tmpName, 0); err != nil {
		return errors.New("artifact fetch recovery temp file 無法清理")
	}
	tmpExists = false
	if err := unix.Fsync(parentFD); err != nil {
		return errors.New("artifact fetch recovery directory 無法 sync")
	}
	if err := validateDiscoveryParent(resolvedParent, parentFD); err != nil {
		return errors.New("artifact fetch recovery parent 在發布時改變")
	}
	return nil
}

func durablyVerifyArtifactFetchRecovery(path string, expected artifactFetchRecovery) error {
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return errors.New("artifact fetch recovery file 在 apply 前無法安全開啟；拒絕 mutation")
	}
	defer unix.Close(parentFD)
	raw, _, err := readPrivateDeploymentRecoveryAt(parentPath, parentFD, filepath.Base(path),
		func(fileFD, parentFD int) error {
			if err := unix.Fsync(fileFD); err != nil {
				return err
			}
			return unix.Fsync(parentFD)
		})
	if err != nil {
		return errors.New("artifact fetch recovery file 在 apply 前無法完成 durability barrier；拒絕 mutation")
	}
	document, err := decodeArtifactFetchRecovery(raw)
	if err != nil || !reflect.DeepEqual(document, expected) {
		return errors.New("artifact fetch recovery file 在 apply 前遺失或改變；拒絕 mutation")
	}
	return nil
}

func removeArtifactFetchRecovery(path string, expected artifactFetchRecovery) error {
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return errors.New("artifact fetch 已成功 enqueue，但 recovery parent 無法安全開啟；receipt 保留")
	}
	defer unix.Close(parentFD)
	name := filepath.Base(path)
	raw, verifiedStat, err := readPrivateDeploymentRecoveryAt(parentPath, parentFD, name, nil)
	if err != nil {
		return errors.New("artifact fetch 已成功 enqueue，但 recovery file 無法安全核對；receipt 保留")
	}
	document, err := decodeArtifactFetchRecovery(raw)
	if err != nil || !reflect.DeepEqual(document, expected) {
		return errors.New("artifact fetch 已成功 enqueue，但 recovery file request 不符；receipt 保留")
	}
	var currentStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &currentStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		validateDeploymentRecoveryStat(&currentStat) != nil ||
		!sameDeploymentRecoveryStat(verifiedStat, currentStat) ||
		validateDiscoveryParent(parentPath, parentFD) != nil {
		return errors.New("artifact fetch 已成功 enqueue，但 recovery file 在 unlink 前改變；receipt 保留")
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return errors.New("artifact fetch 已成功 enqueue，但 recovery file 無法移除；receipt 保留")
	}
	if err := unix.Fsync(parentFD); err != nil {
		return errors.New("artifact fetch 已成功 enqueue，recovery file 已移除但 directory 無法 sync")
	}
	if err := validateDiscoveryParent(parentPath, parentFD); err != nil {
		return errors.New("artifact fetch 已成功 enqueue，recovery parent 在清理時改變")
	}
	return nil
}

func writeArtifactFetchRecoveryInstruction(out io.Writer, path string) error {
	if _, err := fmt.Fprintf(out, "recovery_file %s\n", terminalSafe(path)); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out,
		"replay instruction: clawctl-hub artifact fetch --recovery-file <recovery_file shown above>")
	return err
}
