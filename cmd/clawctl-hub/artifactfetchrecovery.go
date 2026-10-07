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
		return errors.New("invalid artifact fetch recovery file target/confirmation")
	}
	if err := validateArtifactFetchCLIValue("reason", document.Reason, 500, false); err != nil {
		return errors.New("invalid artifact fetch recovery file reason")
	}
	if err := validateArtifactFetchCLIValue("idempotency-key", document.IdempotencyKey, 200, false); err != nil ||
		!validDeploymentPreviewDigest(document.PreviewDigest) {
		return errors.New("invalid artifact fetch recovery file replay coordinates")
	}
	endpoint, err := operatorendpoint.ParseBaseURL(document.HubURL)
	if err != nil || endpoint.BaseURL() != document.HubURL {
		return errors.New("invalid artifact fetch recovery file HTTP authority")
	}
	if document.RequestDigest == "" || document.RequestDigest != artifactFetchRecoveryRequestDigest(document) {
		return errors.New("artifact fetch recovery file request digest mismatch")
	}
	return nil
}

func marshalArtifactFetchRecovery(document artifactFetchRecovery) ([]byte, error) {
	if err := validateArtifactFetchRecovery(document); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, errors.New("failed to encode artifact fetch recovery file")
	}
	raw = append(raw, '\n')
	if len(raw) > artifactFetchRecoveryMaxBytes {
		return nil, errors.New("artifact fetch recovery file too large")
	}
	return raw, nil
}

func decodeArtifactFetchRecovery(raw []byte) (artifactFetchRecovery, error) {
	if len(raw) == 0 || len(raw) > artifactFetchRecoveryMaxBytes {
		return artifactFetchRecovery{}, errors.New("invalid artifact fetch recovery file size")
	}
	var document artifactFetchRecovery
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return artifactFetchRecovery{}, errors.New("invalid artifact fetch recovery file JSON")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return artifactFetchRecovery{}, errors.New("artifact fetch recovery file contains trailing JSON")
	}
	canonical, err := marshalArtifactFetchRecovery(document)
	if err != nil || !bytes.Equal(raw, canonical) {
		return artifactFetchRecovery{}, errors.New("artifact fetch recovery file is not canonical JSON")
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
		return artifactFetchRecovery{}, false, errors.New("artifact fetch: --recovery-file must be a canonical absolute path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return artifactFetchRecovery{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return artifactFetchRecovery{}, false, errors.New("artifact fetch recovery file must be an existing non-symlink regular file")
	}
	raw, err := readPrivateDeploymentRecovery(path)
	if err != nil {
		return artifactFetchRecovery{}, false, errors.New("artifact fetch recovery file must be a 0600 regular file owned by the current user")
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
		return errors.New("artifact fetch recovery file already exists for a different request; refusing to overwrite")
	}
	return writeArtifactFetchRecoveryNoClobber(path, document)
}

func writeArtifactFetchRecoveryNoClobber(path string, document artifactFetchRecovery) error {
	if !validDeploymentRecoveryAbsolutePath(path) {
		return errors.New("artifact fetch: --recovery-file must be a canonical absolute path")
	}
	raw, err := marshalArtifactFetchRecovery(document)
	if err != nil {
		return err
	}
	resolvedParent, parentFD, err := openDeploymentRecoveryParent(path, true)
	if err != nil {
		return errors.New("failed to create or safely open artifact fetch recovery parent")
	}
	defer unix.Close(parentFD)
	tmpName, fd, err := createDeploymentRecoveryTemp(parentFD)
	if err != nil {
		return errors.New("failed to create artifact fetch recovery temporary file")
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
		return errors.New("failed to create artifact fetch recovery temporary handle")
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return errors.New("failed to write artifact fetch recovery file")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("failed to sync artifact fetch recovery file")
	}
	if err := file.Close(); err != nil {
		return errors.New("failed to close artifact fetch recovery file")
	}
	if err := unix.Linkat(parentFD, tmpName, parentFD, filepath.Base(path), 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errors.New("artifact fetch recovery file already exists; refusing to overwrite")
		}
		return errors.New("failed to atomically publish artifact fetch recovery file")
	}
	if err := unix.Unlinkat(parentFD, tmpName, 0); err != nil {
		return errors.New("failed to clean up artifact fetch recovery temporary file")
	}
	tmpExists = false
	if err := unix.Fsync(parentFD); err != nil {
		return errors.New("failed to sync artifact fetch recovery directory")
	}
	if err := validateDiscoveryParent(resolvedParent, parentFD); err != nil {
		return errors.New("artifact fetch recovery parent changed during publication")
	}
	return nil
}

func durablyVerifyArtifactFetchRecovery(path string, expected artifactFetchRecovery) error {
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return errors.New("artifact fetch recovery file cannot be safely opened before apply; refusing mutation")
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
		return errors.New("artifact fetch recovery file failed durability barrier before apply; refusing mutation")
	}
	document, err := decodeArtifactFetchRecovery(raw)
	if err != nil || !reflect.DeepEqual(document, expected) {
		return errors.New("artifact fetch recovery file missing or changed before apply; refusing mutation")
	}
	return nil
}

func removeArtifactFetchRecovery(path string, expected artifactFetchRecovery) error {
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return errors.New("artifact fetch successfully enqueued, but recovery parent cannot be safely opened; receipt retained")
	}
	defer unix.Close(parentFD)
	name := filepath.Base(path)
	raw, verifiedStat, err := readPrivateDeploymentRecoveryAt(parentPath, parentFD, name, nil)
	if err != nil {
		return errors.New("artifact fetch successfully enqueued, but recovery file cannot be safely verified; receipt retained")
	}
	document, err := decodeArtifactFetchRecovery(raw)
	if err != nil || !reflect.DeepEqual(document, expected) {
		return errors.New("artifact fetch successfully enqueued, but recovery file request does not match; receipt retained")
	}
	var currentStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &currentStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		validateDeploymentRecoveryStat(&currentStat) != nil ||
		!sameDeploymentRecoveryStat(verifiedStat, currentStat) ||
		validateDiscoveryParent(parentPath, parentFD) != nil {
		return errors.New("artifact fetch successfully enqueued, but recovery file changed before unlink; receipt retained")
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return errors.New("artifact fetch successfully enqueued, but recovery file cannot be removed; receipt retained")
	}
	if err := unix.Fsync(parentFD); err != nil {
		return errors.New("artifact fetch successfully enqueued, recovery file removed but directory cannot be synced")
	}
	if err := validateDiscoveryParent(parentPath, parentFD); err != nil {
		return errors.New("artifact fetch successfully enqueued, recovery parent changed during cleanup")
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
