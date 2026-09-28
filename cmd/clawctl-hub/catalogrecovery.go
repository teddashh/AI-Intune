package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"golang.org/x/sys/unix"
)

const (
	catalogRecoverySchemaVersion = 1
	catalogRecoveryMaxBytes      = 64 << 10
	catalogRecoveryPackage       = "publish_package"
	catalogRecoveryProfile       = "publish_profile"
	catalogRecoveryAssignment    = "assign_profile"
)

type catalogPackageRecoveryRequest struct {
	Manifest         appcatalog.Manifest `json:"manifest"`
	ConfirmPackageID string              `json:"confirm_package_id"`
	ConfirmVersion   string              `json:"confirm_version"`
	PreviewDigest    string              `json:"preview_digest"`
	Reason           string              `json:"reason"`
}

type catalogProfileRecoveryRequest struct {
	Profile          appcatalog.MachineProfile `json:"profile"`
	ConfirmProfileID string                    `json:"confirm_profile_id"`
	ConfirmRevision  int64                     `json:"confirm_revision"`
	PreviewDigest    string                    `json:"preview_digest"`
	Reason           string                    `json:"reason"`
}

type catalogAssignmentRecoveryRequest struct {
	MachineID          string `json:"machine_id"`
	ProfileID          string `json:"profile_id"`
	ProfileRevision    int64  `json:"profile_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
}

type catalogMutationRecovery struct {
	SchemaVersion  int                               `json:"schema_version"`
	Action         string                            `json:"action"`
	HubURL         string                            `json:"hub_url"`
	IdempotencyKey string                            `json:"idempotency_key"`
	Package        *catalogPackageRecoveryRequest    `json:"package,omitempty"`
	Profile        *catalogProfileRecoveryRequest    `json:"profile,omitempty"`
	Assignment     *catalogAssignmentRecoveryRequest `json:"assignment,omitempty"`
	RequestDigest  string                            `json:"request_digest"`
	ReceiptDigest  string                            `json:"receipt_digest"`
}

func newCatalogPackageRecovery(hubURL, key string,
	request operator.CatalogManifestPublishRequest,
) catalogMutationRecovery {
	document := catalogMutationRecovery{
		SchemaVersion: catalogRecoverySchemaVersion, Action: catalogRecoveryPackage,
		HubURL: hubURL, IdempotencyKey: key,
		Package: &catalogPackageRecoveryRequest{
			Manifest: request.Manifest, ConfirmPackageID: request.ConfirmPackageID,
			ConfirmVersion: request.ConfirmVersion, PreviewDigest: request.PreviewDigest, Reason: request.Reason,
		},
		RequestDigest: operator.CatalogManifestPublishSemanticDigest(request),
	}
	document.ReceiptDigest = catalogRecoveryReceiptDigest(document)
	return document
}

func newCatalogProfileRecovery(hubURL, key string,
	request operator.MachineProfilePublishRequest,
) catalogMutationRecovery {
	document := catalogMutationRecovery{
		SchemaVersion: catalogRecoverySchemaVersion, Action: catalogRecoveryProfile,
		HubURL: hubURL, IdempotencyKey: key,
		Profile: &catalogProfileRecoveryRequest{
			Profile: request.Profile, ConfirmProfileID: request.ConfirmProfileID,
			ConfirmRevision: request.ConfirmRevision, PreviewDigest: request.PreviewDigest, Reason: request.Reason,
		},
		RequestDigest: operator.MachineProfilePublishSemanticDigest(request),
	}
	document.ReceiptDigest = catalogRecoveryReceiptDigest(document)
	return document
}

func newCatalogAssignmentRecovery(hubURL, key string,
	request operator.MachineProfileAssignmentRequest,
) catalogMutationRecovery {
	document := catalogMutationRecovery{
		SchemaVersion: catalogRecoverySchemaVersion, Action: catalogRecoveryAssignment,
		HubURL: hubURL, IdempotencyKey: key,
		Assignment: &catalogAssignmentRecoveryRequest{
			MachineID: request.MachineID, ProfileID: request.ProfileID, ProfileRevision: request.ProfileRevision,
			ConfirmDisplayName: request.ConfirmDisplayName, PreviewDigest: request.PreviewDigest, Reason: request.Reason,
		},
		RequestDigest: operator.MachineProfileAssignmentSemanticDigest(request),
	}
	document.ReceiptDigest = catalogRecoveryReceiptDigest(document)
	return document
}

func catalogRecoveryReceiptDigest(document catalogMutationRecovery) string {
	document.ReceiptDigest = ""
	raw, err := json.Marshal(document)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func marshalCatalogRecovery(document catalogMutationRecovery) ([]byte, error) {
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, errors.New("catalog recovery file 無法編碼")
	}
	return append(raw, '\n'), nil
}

func decodeCatalogRecovery(raw []byte) (catalogMutationRecovery, error) {
	var document catalogMutationRecovery
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, errors.New("catalog recovery file JSON 不合法")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return document, errors.New("catalog recovery file 含 trailing JSON")
	}
	canonical, err := marshalCatalogRecovery(document)
	if err != nil || !bytes.Equal(raw, canonical) {
		return document, errors.New("catalog recovery file 不是 canonical JSON")
	}
	if err := validateCatalogRecovery(document); err != nil {
		return document, err
	}
	return document, nil
}

func validateCatalogRecovery(document catalogMutationRecovery) error {
	if document.SchemaVersion != catalogRecoverySchemaVersion {
		return errors.New("catalog recovery file schema 不支援")
	}
	endpoint, err := operatorendpoint.ParseBaseURL(document.HubURL)
	if err != nil || endpoint.BaseURL() != document.HubURL {
		return errors.New("catalog recovery file Hub URL 不合法")
	}
	if err := validateDeploymentIdempotencyKey(document.IdempotencyKey); err != nil {
		return errors.New("catalog recovery file request key 不合法")
	}
	present := 0
	for _, ok := range []bool{document.Package != nil, document.Profile != nil, document.Assignment != nil} {
		if ok {
			present++
		}
	}
	if present != 1 {
		return errors.New("catalog recovery file request shape 不合法")
	}
	var wanted string
	switch document.Action {
	case catalogRecoveryPackage:
		if document.Package == nil || appcatalog.ValidateManifest(document.Package.Manifest) != nil ||
			document.Package.ConfirmPackageID != document.Package.Manifest.ID ||
			document.Package.ConfirmVersion != document.Package.Manifest.Version ||
			!validCatalogRecoveryDigest(document.Package.PreviewDigest) || validateCatalogReason(document.Package.Reason) != nil {
			return errors.New("catalog recovery package request 不合法")
		}
		wanted = operator.CatalogManifestPublishSemanticDigest(operator.CatalogManifestPublishRequest{
			Manifest: document.Package.Manifest, ConfirmPackageID: document.Package.ConfirmPackageID,
			ConfirmVersion: document.Package.ConfirmVersion, PreviewDigest: document.Package.PreviewDigest,
			Reason: document.Package.Reason,
		})
	case catalogRecoveryProfile:
		if document.Profile == nil || appcatalog.ValidateProfile(document.Profile.Profile) != nil ||
			document.Profile.ConfirmProfileID != document.Profile.Profile.ID ||
			document.Profile.ConfirmRevision != document.Profile.Profile.Revision ||
			!validCatalogRecoveryDigest(document.Profile.PreviewDigest) || validateCatalogReason(document.Profile.Reason) != nil {
			return errors.New("catalog recovery profile request 不合法")
		}
		wanted = operator.MachineProfilePublishSemanticDigest(operator.MachineProfilePublishRequest{
			Profile: document.Profile.Profile, ConfirmProfileID: document.Profile.ConfirmProfileID,
			ConfirmRevision: document.Profile.ConfirmRevision, PreviewDigest: document.Profile.PreviewDigest,
			Reason: document.Profile.Reason,
		})
	case catalogRecoveryAssignment:
		if document.Assignment == nil || validateDeploymentReadCLIValue("machine", document.Assignment.MachineID, 128) != nil ||
			strings.Contains(document.Assignment.MachineID, "/") ||
			validateDeploymentReadCLIValue("profile", document.Assignment.ProfileID, 128) != nil ||
			document.Assignment.ProfileRevision <= 0 ||
			validateDeploymentReadCLIValue("confirm-name", document.Assignment.ConfirmDisplayName, 200) != nil ||
			!validCatalogRecoveryDigest(document.Assignment.PreviewDigest) || validateCatalogReason(document.Assignment.Reason) != nil {
			return errors.New("catalog recovery assignment request 不合法")
		}
		wanted = operator.MachineProfileAssignmentSemanticDigest(operator.MachineProfileAssignmentRequest{
			MachineID: document.Assignment.MachineID, ProfileID: document.Assignment.ProfileID,
			ProfileRevision: document.Assignment.ProfileRevision, ConfirmDisplayName: document.Assignment.ConfirmDisplayName,
			PreviewDigest: document.Assignment.PreviewDigest, Reason: document.Assignment.Reason,
		})
	default:
		return errors.New("catalog recovery file action 不合法")
	}
	if document.RequestDigest != wanted || document.ReceiptDigest != catalogRecoveryReceiptDigest(document) {
		return errors.New("catalog recovery file digest 不符")
	}
	return nil
}

func validCatalogRecoveryDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && artifact.ValidSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}

func applyCatalogMutationWithRecovery(document catalogMutationRecovery, requestedPath string,
	errOut io.Writer, apply func() (any, error), writeResult func(any) error,
) error {
	if apply == nil || writeResult == nil {
		return errors.New("catalog mutation 未初始化")
	}
	path := requestedPath
	var err error
	if path == "" {
		path, err = defaultDeploymentRecoveryPath("catalog-"+document.Action, document.IdempotencyKey)
		if err != nil {
			return err
		}
	}
	if err := ensureCatalogRecovery(path, document); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(errOut, "recovery_file %s\nreplay clawctl-hub catalog recover --recovery-file %s\n",
		terminalSafe(path), terminalSafe(path)); err != nil {
		return err
	}
	result, applyErr := apply()
	if applyErr != nil {
		if catalogRecoveryIsDefinitive(applyErr) {
			if removeErr := removeCatalogRecovery(path, document); removeErr != nil {
				return errors.Join(applyErr, removeErr)
			}
		}
		return applyErr
	}
	if err := writeResult(result); err != nil {
		return fmt.Errorf("操作已完成，結果輸出失敗；請使用 recovery 檔重試：%w", err)
	}
	if err := removeCatalogRecovery(path, document); err != nil {
		return err
	}
	return nil
}

func runCatalogRecover(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("catalog recover", flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("recovery-file", "", "private canonical replay receipt path")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 || !validDeploymentRecoveryAbsolutePath(*path) {
		return errors.New("catalog recover: --recovery-file 必須是 canonical absolute path")
	}
	document, err := loadCatalogRecovery(*path)
	if err != nil {
		return err
	}
	if deps.newOperatorClient == nil {
		return errors.New("catalog recover: operator HTTP client 未初始化")
	}
	client, err := deps.newOperatorClient(document.HubURL)
	if err != nil {
		return fmt.Errorf("catalog recover: 連接 Hub 失敗：%w", err)
	}
	return applyCatalogMutationWithRecovery(document, *path, errOut, func() (any, error) {
		result, err := replayCatalogRecovery(ctx, client, document)
		if err != nil {
			return nil, fmt.Errorf("catalog recover 失敗：%w", err)
		}
		return result, nil
	}, func(result any) error {
		if *jsonOutput {
			return writeCatalogJSON(out, result)
		}
		var err error
		switch value := result.(type) {
		case operator.CatalogManifestPublishResult:
			_, err = fmt.Fprintf(out, "published %s@%s digest=%s replayed=%t\n", value.Record.Manifest.ID,
				value.Record.Manifest.Version, value.Record.Digest, value.Replayed)
		case operator.MachineProfilePublishResult:
			_, err = fmt.Fprintf(out, "published %s@%d digest=%s replayed=%t\n", value.Record.Profile.ID,
				value.Record.Profile.Revision, value.Record.Digest, value.Replayed)
		case operator.MachineProfileAssignmentResult:
			err = writeCatalogAssignmentResult(out, value)
		default:
			return errors.New("catalog recover 回應型別不符")
		}
		return err
	})
}

func replayCatalogRecovery(ctx context.Context, client *operatorclient.Client,
	document catalogMutationRecovery,
) (any, error) {
	switch document.Action {
	case catalogRecoveryPackage:
		request := document.Package
		return client.PublishStandardCatalogManifest(ctx, document.IdempotencyKey, operator.CatalogManifestPublishRequest{
			Manifest: request.Manifest, ConfirmPackageID: request.ConfirmPackageID, ConfirmVersion: request.ConfirmVersion,
			PreviewDigest: request.PreviewDigest, Reason: request.Reason,
		})
	case catalogRecoveryProfile:
		request := document.Profile
		return client.PublishReviewedMachineProfile(ctx, document.IdempotencyKey, operator.MachineProfilePublishRequest{
			Profile: request.Profile, ConfirmProfileID: request.ConfirmProfileID, ConfirmRevision: request.ConfirmRevision,
			PreviewDigest: request.PreviewDigest, Reason: request.Reason,
		})
	case catalogRecoveryAssignment:
		request := document.Assignment
		return client.AssignMachineProfile(ctx, document.IdempotencyKey, operator.MachineProfileAssignmentRequest{
			MachineID: request.MachineID, ProfileID: request.ProfileID, ProfileRevision: request.ProfileRevision,
			ConfirmDisplayName: request.ConfirmDisplayName, PreviewDigest: request.PreviewDigest, Reason: request.Reason,
		})
	default:
		return nil, errors.New("catalog recovery action 不合法")
	}
}

func loadCatalogRecovery(path string) (catalogMutationRecovery, error) {
	if !validDeploymentRecoveryAbsolutePath(path) {
		return catalogMutationRecovery{}, errors.New("catalog recovery path 不合法")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return catalogMutationRecovery{}, errors.New("catalog recovery file 不存在或不是 regular file")
	}
	raw, err := readPrivateDeploymentRecovery(path)
	if err != nil {
		return catalogMutationRecovery{}, err
	}
	return decodeCatalogRecovery(raw)
}

func ensureCatalogRecovery(path string, document catalogMutationRecovery) error {
	if err := validateCatalogRecovery(document); err != nil {
		return err
	}
	canonical, err := marshalCatalogRecovery(document)
	if err != nil {
		return err
	}
	if existing, loadErr := loadCatalogRecovery(path); loadErr == nil {
		if reflect.DeepEqual(existing, document) {
			return nil
		}
		return errors.New("catalog recovery file 已存在且 request 不同")
	} else if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		return loadErr
	}
	return writeCatalogRecoveryNoClobber(path, canonical)
}

func writeCatalogRecoveryNoClobber(path string, raw []byte) error {
	if !validDeploymentRecoveryAbsolutePath(path) || len(raw) > catalogRecoveryMaxBytes {
		return errors.New("catalog recovery file path 或大小不合法")
	}
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, true)
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
	file := os.NewFile(uintptr(fd), "catalog-recovery-temp")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("catalog recovery temp handle 無法建立")
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return errors.New("catalog recovery temp file 無法寫入")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("catalog recovery temp file 無法 sync")
	}
	if err := file.Close(); err != nil {
		return errors.New("catalog recovery temp file 無法關閉")
	}
	if err := unix.Linkat(parentFD, tmpName, parentFD, filepath.Base(path), 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errors.New("catalog recovery file 已存在且 request 不同")
		}
		return errors.New("catalog recovery file 無法發布")
	}
	if err := unix.Unlinkat(parentFD, tmpName, 0); err != nil {
		return errors.New("catalog recovery temp file 無法清除")
	}
	tmpExists = false
	if err := unix.Fsync(parentFD); err != nil || validateDiscoveryParent(parentPath, parentFD) != nil {
		return errors.New("catalog recovery file 無法確認落盤")
	}
	return nil
}

func removeCatalogRecovery(path string, expected catalogMutationRecovery) error {
	parentPath, parentFD, err := openDeploymentRecoveryParent(path, false)
	if err != nil {
		return errors.New("catalog mutation 已完成；recovery parent 無法開啟")
	}
	defer unix.Close(parentFD)
	name := filepath.Base(path)
	raw, verifiedStat, err := readPrivateDeploymentRecoveryAt(parentPath, parentFD, name, nil)
	if err != nil {
		return errors.New("catalog mutation 已完成；recovery file 無法核對")
	}
	document, err := decodeCatalogRecovery(raw)
	if err != nil || !reflect.DeepEqual(document, expected) {
		return errors.New("catalog mutation 已完成；recovery request 不符")
	}
	var currentStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &currentStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		validateDeploymentRecoveryStat(&currentStat) != nil || !sameDeploymentRecoveryStat(verifiedStat, currentStat) ||
		validateDiscoveryParent(parentPath, parentFD) != nil {
		return errors.New("catalog mutation 已完成；recovery file identity 已改變")
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil || unix.Fsync(parentFD) != nil {
		return errors.New("catalog mutation 已完成；recovery file 無法清除")
	}
	return nil
}

func catalogRecoveryIsDefinitive(err error) bool {
	var apiErr *operatorclient.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode >= http.StatusBadRequest && apiErr.StatusCode < http.StatusInternalServerError
}
