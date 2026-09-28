package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"github.com/teddashh/AI-Intune/internal/store"
	"golang.org/x/sys/unix"
)

const (
	deploymentRecoverySchemaVersion = 1
	deploymentRecoveryMaxBytes      = 16 << 10
	deploymentRecoveryHTTP          = "http"
	deploymentRecoveryDirectDB      = "direct_db"
)

type deploymentRecoveryTransport struct {
	Mode   string `json:"mode"`
	HubURL string `json:"hub_url"`
	DBPath string `json:"db_path"`
}

// deploymentMutationRecovery is a private, canonical replay receipt. It keeps
// the exact request inputs, including an intentionally omitted artifact SHA
// (the empty string) and the audit reason, without copying either to terminal
// output. RequestDigest binds the whole replay receipt (including its key and
// exact transport) against accidental edits before any network or Store
// access; the Hub remains the final idempotency authority.
type deploymentMutationRecovery struct {
	SchemaVersion           int                                     `json:"schema_version"`
	Action                  string                                  `json:"action"`
	DeploymentID            string                                  `json:"deployment_id"`
	Planning                operator.DeploymentCreatePreviewRequest `json:"planning"`
	ConfirmChannel          string                                  `json:"confirm_channel"`
	ConfirmVersion          string                                  `json:"confirm_version"`
	ConfirmDeploymentID     string                                  `json:"confirm_deployment_id"`
	Reason                  string                                  `json:"reason"`
	IdempotencyKey          string                                  `json:"idempotency_key"`
	PreviewDigest           string                                  `json:"preview_digest"`
	ExpectedControlRevision *int64                                  `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int                                    `json:"expected_opened_batch"`
	RequestDigest           string                                  `json:"request_digest"`
	Transport               deploymentRecoveryTransport             `json:"transport"`
}

func deploymentRecoveryFromInputs(inputs deploymentMutationInputs) deploymentMutationRecovery {
	document := deploymentMutationRecovery{
		SchemaVersion: deploymentRecoverySchemaVersion,
		Action:        inputs.Action, DeploymentID: inputs.DeploymentID, Planning: inputs.Planning,
		ConfirmChannel: inputs.ConfirmChannel, ConfirmVersion: inputs.ConfirmVersion,
		ConfirmDeploymentID: inputs.ConfirmDeploymentID, Reason: inputs.Reason,
		IdempotencyKey: inputs.IdempotencyKey, PreviewDigest: inputs.PreviewDigest,
		ExpectedControlRevision: inputs.ExpectedControlRevision, ExpectedOpenedBatch: inputs.ExpectedOpenedBatch,
		Transport: inputs.RecoveryTransport,
	}
	document.RequestDigest = deploymentRecoveryRequestDigest(document)
	return document
}

// deploymentRecoveryRequestDigest deliberately hashes the canonical receipt
// with only the digest value cleared. JSON-output preference is not part of
// the receipt and therefore does not change the replay request.
func deploymentRecoveryRequestDigest(document deploymentMutationRecovery) string {
	document.RequestDigest = ""
	raw, err := json.Marshal(document)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deploymentInputsFromRecovery(document deploymentMutationRecovery, path string,
	jsonOutput bool,
) (deploymentMutationInputs, deploymentReadTransport) {
	inputs := deploymentMutationInputs{
		Action: document.Action, DeploymentID: document.DeploymentID, Planning: document.Planning,
		ConfirmChannel: document.ConfirmChannel, ConfirmVersion: document.ConfirmVersion,
		ConfirmDeploymentID: document.ConfirmDeploymentID, Reason: document.Reason,
		IdempotencyKey: document.IdempotencyKey, PreviewDigest: document.PreviewDigest,
		ExpectedControlRevision: document.ExpectedControlRevision, ExpectedOpenedBatch: document.ExpectedOpenedBatch,
		JSON: jsonOutput, RecoveryFile: path, RecoveryLoaded: true, RecoveryTransport: document.Transport,
	}
	transport := deploymentReadTransport{json: jsonOutput, explicit: make(map[string]bool)}
	if document.Transport.Mode == deploymentRecoveryDirectDB {
		transport.dbPath = document.Transport.DBPath
		transport.explicit["db"] = true
	} else {
		transport.hubURL = document.Transport.HubURL
		transport.explicit["hub-url"] = true
	}
	return inputs, transport
}

func validateDeploymentRecoveryDocument(document deploymentMutationRecovery) error {
	if document.SchemaVersion != deploymentRecoverySchemaVersion {
		return errors.New("deployment recovery file schema 不支援")
	}
	if document.Action != "create" && document.Action != "continue" &&
		document.Action != "retry" && document.Action != "abandon" {
		return errors.New("deployment recovery file action 不合法")
	}
	if err := validateDeploymentIdempotencyKey(document.IdempotencyKey); err != nil ||
		!validDeploymentPreviewDigest(document.PreviewDigest) {
		return errors.New("deployment recovery file replay coordinates 不合法")
	}
	if document.Reason != strings.TrimSpace(document.Reason) || len(document.Reason) > 500 ||
		containsDeploymentControl(document.Reason) {
		return errors.New("deployment recovery file reason 不合法")
	}

	zeroPlanning := operator.DeploymentCreatePreviewRequest{}
	switch document.Action {
	case "create":
		if document.DeploymentID != "" || document.ExpectedControlRevision != nil ||
			document.ExpectedOpenedBatch != nil || document.ConfirmDeploymentID != "" ||
			(document.Planning.Channel != "canary" && document.Planning.Channel != "stable") ||
			validateDeploymentReadCLIValue("version", document.Planning.Version, 128) != nil ||
			(document.Planning.ArtifactSHA256 != "" && !artifact.ValidSHA256Hex(document.Planning.ArtifactSHA256)) ||
			document.Planning.BatchSize < 1 || document.Planning.BatchSize > store.MaxDeploymentBatchSize ||
			document.Planning.ExecutionTimeoutSeconds < 1 || document.Planning.ExecutionTimeoutSeconds > 86400 ||
			document.ConfirmChannel != document.Planning.Channel || document.ConfirmVersion != document.Planning.Version {
			return errors.New("deployment recovery file create request 不合法")
		}
	case "continue", "retry", "abandon":
		if err := validateDeploymentIdentifierCLI(document.DeploymentID); err != nil ||
			document.Planning != zeroPlanning || document.ExpectedControlRevision == nil ||
			document.ExpectedOpenedBatch == nil || *document.ExpectedControlRevision < 0 ||
			*document.ExpectedOpenedBatch < 1 || *document.ExpectedControlRevision == store.MaxDeploymentControlRevision ||
			*document.ExpectedOpenedBatch == int(^uint(0)>>1) {
			return errors.New("deployment recovery file action request 不合法")
		}
		if document.Action == "continue" &&
			((document.ConfirmChannel != "canary" && document.ConfirmChannel != "stable") ||
				document.ConfirmVersion != "" || document.ConfirmDeploymentID != "") {
			return errors.New("deployment recovery file continue confirmation 不合法")
		}
		if document.Action == "retry" &&
			((document.ConfirmChannel != "canary" && document.ConfirmChannel != "stable") ||
				validateDeploymentReadCLIValue("confirm-version", document.ConfirmVersion, 128) != nil ||
				document.ConfirmDeploymentID != "") {
			return errors.New("deployment recovery file retry confirmation 不合法")
		}
		if document.Action == "abandon" &&
			(document.ConfirmChannel != "" || document.ConfirmVersion != "" ||
				document.ConfirmDeploymentID != document.DeploymentID) {
			return errors.New("deployment recovery file abandon confirmation 不合法")
		}
	}

	switch document.Transport.Mode {
	case deploymentRecoveryHTTP:
		endpoint, err := operatorendpoint.ParseBaseURL(document.Transport.HubURL)
		if err != nil || endpoint.BaseURL() != document.Transport.HubURL || document.Transport.DBPath != "" {
			return errors.New("deployment recovery file HTTP authority 不合法")
		}
	case deploymentRecoveryDirectDB:
		if document.Transport.HubURL != "" || !validDeploymentRecoveryAbsolutePath(document.Transport.DBPath) {
			return errors.New("deployment recovery file direct DB target 不合法")
		}
	default:
		return errors.New("deployment recovery file transport 不合法")
	}
	if document.RequestDigest == "" || document.RequestDigest != deploymentRecoveryRequestDigest(document) {
		return errors.New("deployment recovery file request digest 不符")
	}
	return nil
}

func marshalDeploymentRecovery(document deploymentMutationRecovery) ([]byte, error) {
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, errors.New("deployment recovery file 無法編碼")
	}
	return append(raw, '\n'), nil
}

func decodeDeploymentRecovery(raw []byte) (deploymentMutationRecovery, error) {
	var document deploymentMutationRecovery
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return deploymentMutationRecovery{}, errors.New("deployment recovery file JSON 不合法")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return deploymentMutationRecovery{}, errors.New("deployment recovery file 含 trailing JSON")
	}
	canonical, err := marshalDeploymentRecovery(document)
	if err != nil || !bytes.Equal(raw, canonical) {
		return deploymentMutationRecovery{}, errors.New("deployment recovery file 不是 canonical JSON")
	}
	if err := validateDeploymentRecoveryDocument(document); err != nil {
		return deploymentMutationRecovery{}, err
	}
	return document, nil
}

func validDeploymentRecoveryAbsolutePath(path string) bool {
	return path != "" && path == strings.TrimSpace(path) && len(path) <= 4096 &&
		filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Base(path) != "." &&
		filepath.Base(path) != string(filepath.Separator) && !containsDeploymentControl(path)
}

func loadDeploymentRecoveryIfExists(path string) (deploymentMutationRecovery, bool, error) {
	if !validDeploymentRecoveryAbsolutePath(path) {
		return deploymentMutationRecovery{}, false, errors.New("deployment --recovery-file 必須是 canonical absolute path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return deploymentMutationRecovery{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return deploymentMutationRecovery{}, false, errors.New("deployment recovery file 必須是既有 non-symlink regular file")
	}
	raw, err := readPrivateDeploymentRecovery(path)
	if err != nil {
		return deploymentMutationRecovery{}, false, err
	}
	document, err := decodeDeploymentRecovery(raw)
	if err != nil {
		return deploymentMutationRecovery{}, false, err
	}
	return document, true, nil
}

func readPrivateDeploymentRecovery(path string) ([]byte, error) {
	resolvedParent, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)
	raw, _, err := readPrivateDeploymentRecoveryAt(resolvedParent, parentFD, filepath.Base(path), nil)
	return raw, err
}

func readPrivateDeploymentRecoveryAt(parentPath string, parentFD int, name string,
	syncReceipt func(fileFD, parentFD int) error,
) ([]byte, unix.Stat_t, error) {
	fd, err := unix.Openat(parentFD, name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, unix.Stat_t{}, errors.New("deployment recovery file 無法安全開啟")
	}
	f := os.NewFile(uintptr(fd), "deployment-recovery")
	if f == nil {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, errors.New("deployment recovery file 無法建立 handle")
	}
	defer f.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || validateDeploymentRecoveryStat(&before) != nil {
		return nil, unix.Stat_t{}, errors.New("deployment recovery file owner/type/mode 不合法；必須為目前使用者的 0600 regular file")
	}
	if before.Size > deploymentRecoveryMaxBytes {
		return nil, unix.Stat_t{}, errors.New("deployment recovery file 過大")
	}
	raw, err := io.ReadAll(io.LimitReader(f, deploymentRecoveryMaxBytes+1))
	if err != nil || len(raw) > deploymentRecoveryMaxBytes {
		return nil, unix.Stat_t{}, errors.New("deployment recovery file 無法安全讀取")
	}
	var pathStat, after unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		unix.Fstat(fd, &after) != nil || validateDeploymentRecoveryStat(&pathStat) != nil ||
		validateDeploymentRecoveryStat(&after) != nil || !sameDeploymentRecoveryStat(before, pathStat) ||
		!sameDeploymentRecoveryStat(before, after) {
		return nil, unix.Stat_t{}, errors.New("deployment recovery file 在讀取時改變")
	}
	if syncReceipt != nil {
		if err := syncReceipt(fd, parentFD); err != nil {
			return nil, unix.Stat_t{}, errors.New("deployment recovery file durability barrier 失敗")
		}
		var durablePath, durableFile unix.Stat_t
		if err := unix.Fstatat(parentFD, name, &durablePath, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
			unix.Fstat(fd, &durableFile) != nil || !sameDeploymentRecoveryStat(before, durablePath) ||
			!sameDeploymentRecoveryStat(before, durableFile) {
			return nil, unix.Stat_t{}, errors.New("deployment recovery file 在 durability barrier 時改變")
		}
	}
	if err := validateDiscoveryParent(parentPath, parentFD); err != nil {
		return nil, unix.Stat_t{}, errors.New("deployment recovery file parent 在讀取時改變")
	}
	return raw, before, nil
}

func validateDeploymentRecoveryStat(stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) ||
		stat.Nlink != 1 || stat.Mode&0o7777 != 0o600 {
		return errors.New("invalid private recovery file")
	}
	return nil
}

func sameDeploymentRecoveryStat(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid &&
		a.Gid == b.Gid && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

func defaultDeploymentRecoveryPath(action, key string) (string, error) {
	return defaultDeploymentRecoveryPathWithSync(action, key, unix.Fsync)
}

func defaultDeploymentRecoveryPathWithSync(action, key string, syncDirectory func(int) error) (string, error) {
	root, present := os.LookupEnv("XDG_STATE_HOME")
	if present {
		if !validDeploymentRecoveryAbsolutePath(root) {
			return "", errors.New("XDG_STATE_HOME 必須是 canonical absolute path")
		}
	} else {
		home, err := os.UserHomeDir()
		if err != nil || !validDeploymentRecoveryAbsolutePath(home) {
			return "", errors.New("無法取得 canonical user home 來建立 deployment recovery")
		}
		root = filepath.Join(home, ".local", "state")
	}
	rootFD, err := openDurableDeploymentRecoveryDir(root, true, syncDirectory)
	if err != nil {
		return "", errors.New("deployment recovery state root 必須可安全建立且由目前使用者持有")
	}
	if err := unix.Close(rootFD); err != nil {
		return "", errors.New("deployment recovery state root 無法安全關閉")
	}
	dir := filepath.Join(root, "clawctl", "deployment-recovery")
	if err := ensureDurableDeploymentRecoveryDir(dir, syncDirectory); err != nil {
		return "", errors.New("無法建立 deployment recovery state directory")
	}
	var stat unix.Stat_t
	if err := unix.Lstat(dir, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o7777 != 0o700 {
		return "", errors.New("deployment recovery state directory 必須由目前使用者持有且為 0700")
	}
	nameDigest := sha256.Sum256([]byte(action + "\x00" + key))
	return filepath.Join(dir, hex.EncodeToString(nameDigest[:])+".json"), nil
}

// ensureDurableDeploymentRecoveryDir walks from the filesystem root with
// no-follow descriptors. Each directory created by this call is initialized,
// synced, and then made durable by syncing its parent before the next path
// component is created. This prevents a durable recovery file from living in
// a directory chain whose entries can disappear after a local crash.
func ensureDurableDeploymentRecoveryDir(path string, syncDirectory func(int) error) error {
	fd, err := openDurableDeploymentRecoveryDir(path, true, syncDirectory)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func openDurableDeploymentRecoveryDir(path string, create bool, syncDirectory func(int) error) (int, error) {
	if !validDeploymentRecoveryAbsolutePath(path) || syncDirectory == nil {
		return -1, errors.New("deployment recovery directory path 不合法")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, errors.New("deployment recovery filesystem root 無法安全開啟")
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return -1, errors.New("deployment recovery directory component 不合法")
		}
		var pathStat unix.Stat_t
		created := false
		for {
			err = unix.Fstatat(fd, component, &pathStat, unix.AT_SYMLINK_NOFOLLOW)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.ENOENT) {
				return -1, errors.New("deployment recovery directory component 無法檢查")
			}
			if !create {
				return -1, errors.New("deployment recovery directory component 不存在")
			}
			if err := unix.Mkdirat(fd, component, 0o700); err != nil {
				if errors.Is(err, unix.EEXIST) {
					continue
				}
				return -1, errors.New("deployment recovery directory component 無法建立")
			}
			created = true
			if err := unix.Fstatat(fd, component, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return -1, errors.New("deployment recovery new directory 無法檢查")
			}
			break
		}
		if pathStat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return -1, errors.New("deployment recovery directory chain 不可含 symlink 或非 directory")
		}
		childFD, err := unix.Openat(fd, component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return -1, errors.New("deployment recovery directory component 無法安全開啟")
		}
		var fdStat unix.Stat_t
		if err := unix.Fstat(childFD, &fdStat); err != nil ||
			pathStat.Dev != fdStat.Dev || pathStat.Ino != fdStat.Ino {
			_ = unix.Close(childFD)
			return -1, errors.New("deployment recovery directory path/fd identity 改變")
		}
		if created {
			if err := unix.Fchmod(childFD, 0o700); err != nil {
				_ = unix.Close(childFD)
				return -1, errors.New("deployment recovery new directory mode 無法固定為 0700")
			}
		}
		// This is deliberately unconditional. A prior invocation can have
		// created the entry and then failed its fsync; seeing it as existing on
		// retry is not evidence that the directory chain is durable.
		if err := syncDirectory(childFD); err != nil {
			_ = unix.Close(childFD)
			return -1, errors.New("deployment recovery directory 無法 sync")
		}
		if err := syncDirectory(fd); err != nil {
			_ = unix.Close(childFD)
			return -1, errors.New("deployment recovery directory parent 無法 sync")
		}
		if err := unix.Fstat(childFD, &fdStat); err != nil ||
			fdStat.Mode&unix.S_IFMT != unix.S_IFDIR ||
			(created && (fdStat.Uid != uint32(os.Geteuid()) || fdStat.Mode&0o7777 != 0o700)) {
			_ = unix.Close(childFD)
			return -1, errors.New("deployment recovery directory owner/type/mode 不合法")
		}
		if index == len(components)-1 {
			if err := validateDeploymentRecoveryFinalDirectory(&fdStat); err != nil {
				_ = unix.Close(childFD)
				return -1, err
			}
		}
		_ = unix.Close(fd)
		fd = childFD
	}
	result := fd
	fd = -1
	return result, nil
}

func validateDeploymentRecoveryFinalDirectory(stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("deployment recovery path component 不是 directory")
	}
	currentUID := uint32(os.Geteuid())
	if stat.Uid != currentUID {
		return errors.New("deployment recovery final directory 必須由目前使用者持有")
	}
	if stat.Mode&0o022 != 0 {
		return errors.New("deployment recovery final directory 不可讓 group/other 寫入")
	}
	return nil
}

func ensureDeploymentRecovery(path string, document deploymentMutationRecovery, alreadyLoaded bool) error {
	if alreadyLoaded {
		return verifyDeploymentRecovery(path, document)
	}
	existing, exists, err := loadDeploymentRecoveryIfExists(path)
	if err != nil {
		return err
	}
	if exists {
		if reflect.DeepEqual(existing, document) {
			return nil
		}
		return errors.New("deployment recovery file 已存在且屬於不同 request；拒絕覆寫")
	}
	return writeDeploymentRecoveryNoClobber(path, document)
}

func verifyDeploymentRecovery(path string, expected deploymentMutationRecovery) error {
	document, exists, err := loadDeploymentRecoveryIfExists(path)
	if err != nil || !exists || !reflect.DeepEqual(document, expected) {
		return errors.New("deployment recovery file 在 apply 前遺失或改變；拒絕 mutation")
	}
	return nil
}

func durablyVerifyDeploymentRecovery(path string, expected deploymentMutationRecovery) error {
	return durablyVerifyDeploymentRecoveryWithSync(path, expected, unix.Fsync)
}

func durablyVerifyDeploymentRecoveryWithSync(path string, expected deploymentMutationRecovery,
	syncFD func(int) error,
) error {
	if syncFD == nil {
		return errors.New("deployment recovery durability barrier 未初始化")
	}
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return errors.New("deployment recovery file 在 apply 前無法安全開啟；拒絕 mutation")
	}
	defer unix.Close(parentFD)
	raw, _, err := readPrivateDeploymentRecoveryAt(parentPath, parentFD, filepath.Base(path),
		func(fileFD, parentFD int) error {
			if err := syncFD(fileFD); err != nil {
				return err
			}
			return syncFD(parentFD)
		})
	if err != nil {
		return errors.New("deployment recovery file 在 apply 前無法完成 durability barrier；拒絕 mutation")
	}
	document, err := decodeDeploymentRecovery(raw)
	if err != nil || !reflect.DeepEqual(document, expected) {
		return errors.New("deployment recovery file 在 apply 前遺失或改變；拒絕 mutation")
	}
	return nil
}

func writeDeploymentRecoveryNoClobber(path string, document deploymentMutationRecovery) error {
	return writeDeploymentRecoveryNoClobberWithSync(path, document, unix.Fsync)
}

func writeDeploymentRecoveryNoClobberWithSync(path string, document deploymentMutationRecovery,
	syncDirectory func(int) error,
) error {
	if !validDeploymentRecoveryAbsolutePath(path) {
		return errors.New("deployment --recovery-file 必須是 canonical absolute path")
	}
	if syncDirectory == nil {
		return errors.New("deployment recovery directory sync 未初始化")
	}
	raw, err := marshalDeploymentRecovery(document)
	if err != nil {
		return err
	}
	if len(raw) > deploymentRecoveryMaxBytes {
		return errors.New("deployment recovery file 過大")
	}
	resolvedParent, parentFD, err := openDeploymentRecoveryParent(path, true)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)

	tmpName, fd, err := createDeploymentRecoveryTemp(parentFD)
	if err != nil {
		return err
	}
	tmpExists := true
	defer func() {
		if tmpExists {
			_ = unix.Unlinkat(parentFD, tmpName, 0)
		}
	}()
	f := os.NewFile(uintptr(fd), "deployment-recovery-temp")
	if f == nil {
		_ = unix.Close(fd)
		return errors.New("deployment recovery temp file 無法建立 handle")
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return errors.New("deployment recovery temp file 無法寫入")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return errors.New("deployment recovery temp file 無法 sync")
	}
	if err := f.Close(); err != nil {
		return errors.New("deployment recovery temp file 無法關閉")
	}
	if err := unix.Linkat(parentFD, tmpName, parentFD, filepath.Base(path), 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errors.New("deployment recovery file 已存在；拒絕覆寫")
		}
		return errors.New("deployment recovery file 無法原子發布")
	}
	if err := unix.Unlinkat(parentFD, tmpName, 0); err != nil {
		return errors.New("deployment recovery temp file 無法清理")
	}
	tmpExists = false
	if err := syncDirectory(parentFD); err != nil {
		return errors.New("deployment recovery directory 無法 sync")
	}
	if err := validateDiscoveryParent(resolvedParent, parentFD); err != nil {
		return errors.New("deployment recovery parent 在發布時改變")
	}
	return nil
}

func createDeploymentRecoveryTemp(parentFD int) (string, int, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", -1, errors.New("deployment recovery temp name 無法產生")
		}
		name := ".deployment-recovery-" + hex.EncodeToString(nonce[:]) + ".tmp"
		fd, err := unix.Openat(parentFD, name,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err == nil {
			if err := unix.Fchmod(fd, 0o600); err != nil {
				_ = unix.Close(fd)
				_ = unix.Unlinkat(parentFD, name, 0)
				return "", -1, errors.New("deployment recovery temp mode 無法固定為 0600")
			}
			return name, fd, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", -1, errors.New("deployment recovery temp file 無法建立")
		}
	}
	return "", -1, errors.New("deployment recovery temp name collision")
}

func openDeploymentRecoveryParent(path string, create bool) (string, int, error) {
	parent := filepath.Dir(path)
	fd, err := openDurableDeploymentRecoveryDir(parent, create, unix.Fsync)
	if err != nil {
		return "", -1, errors.New("deployment recovery parent 無法建立或安全開啟")
	}
	if err := validateDiscoveryParent(parent, fd); err != nil {
		_ = unix.Close(fd)
		return "", -1, errors.New("deployment recovery parent 必須由目前使用者持有且不可讓 group/other 寫入")
	}
	return parent, fd, nil
}

func removeDeploymentRecovery(path string, expected deploymentMutationRecovery) error {
	return removeDeploymentRecoveryWithHook(path, expected, nil)
}

func removeDeploymentRecoveryWithHook(path string, expected deploymentMutationRecovery, beforeUnlink func()) error {
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return errors.New("已成功套用 deployment，但 recovery file parent 無法安全開啟；未執行刪除")
	}
	defer unix.Close(parentFD)
	name := filepath.Base(path)
	raw, verifiedStat, err := readPrivateDeploymentRecoveryAt(parentPath, parentFD, name, nil)
	if err != nil {
		return errors.New("已成功套用 deployment，但 recovery file 無法安全核對；未刪除任何已核對檔案")
	}
	document, err := decodeDeploymentRecovery(raw)
	if err != nil || !reflect.DeepEqual(document, expected) {
		return errors.New("已成功套用 deployment，但 recovery file request 不符；未執行刪除")
	}
	if beforeUnlink != nil {
		beforeUnlink()
	}
	var currentStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &currentStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		validateDeploymentRecoveryStat(&currentStat) != nil ||
		!sameDeploymentRecoveryStat(verifiedStat, currentStat) {
		return errors.New("已成功套用 deployment，但 recovery file 在 unlink 前改變；未執行刪除")
	}
	if err := validateDiscoveryParent(parentPath, parentFD); err != nil {
		return errors.New("已成功套用 deployment，但 recovery parent 在 unlink 前改變；未執行刪除")
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return errors.New("已成功套用 deployment，但 recovery file unlink 未確認成功；請先檢查 recovery path 再決定是否 replay")
	}
	if err := unix.Fsync(parentFD); err != nil {
		return errors.New("已成功套用 deployment，recovery file 已 unlink，但 recovery directory 無法 sync")
	}
	if err := validateDiscoveryParent(parentPath, parentFD); err != nil {
		return errors.New("已成功套用 deployment，recovery file 已 unlink，但 recovery parent 在清理時改變")
	}
	return nil
}

func writeDeploymentRecoveryInstruction(w io.Writer, action, path string) error {
	if _, err := fmt.Fprintf(w, "recovery_file %s\n", terminalSafe(path)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w,
		"replay instruction: clawctl-hub deployment %s --recovery-file <recovery_file shown above>\n", action)
	return err
}
