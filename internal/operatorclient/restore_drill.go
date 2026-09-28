package operatorclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type RestoreDrillApplyRequest struct {
	PreviewDigest string `json:"preview_digest"`
	Confirm       string `json:"confirm"`
	Reason        string `json:"reason"`
}

func (c *Client) PreviewRestoreDrill(ctx context.Context) (operator.RestoreDrillPreview, error) {
	raw := []byte(`{}`)
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/maintenance/restore-drill-preview", bytes.NewReader(raw))
	if err != nil {
		return operator.RestoreDrillPreview{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return operator.RestoreDrillPreview{}, err
	}
	if response.status != http.StatusOK {
		return operator.RestoreDrillPreview{}, fmt.Errorf("operator client: restore drill preview returned HTTP %d", response.status)
	}
	if err := validateRestoreDrillHeaders(response.header, false); err != nil {
		return operator.RestoreDrillPreview{}, err
	}
	var result operator.RestoreDrillPreview
	if err := decodeStrictJSONDocument(response.body, "restore drill preview", &result); err != nil {
		return result, err
	}
	if err := validateRestoreDrillPreview(result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) CreateRestoreDrill(ctx context.Context, key string, body RestoreDrillApplyRequest) (store.OperatorRestoreDrillResult, error) {
	if strings.TrimSpace(key) != key || key == "" || len(key) > 200 || !validSHA256Digest(body.PreviewDigest) ||
		body.Confirm == "" || !validTailnetReason(body.Reason) {
		return store.OperatorRestoreDrillResult{}, errors.New("operator client: restore drill apply coordinates are invalid")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return store.OperatorRestoreDrillResult{}, err
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/maintenance/restore-drills", bytes.NewReader(raw))
	if err != nil {
		return store.OperatorRestoreDrillResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	response, err := c.doRaw(req)
	if err != nil {
		return store.OperatorRestoreDrillResult{}, err
	}
	if response.status != http.StatusAccepted && response.status != http.StatusOK {
		return store.OperatorRestoreDrillResult{}, fmt.Errorf("operator client: restore drill create returned HTTP %d", response.status)
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return store.OperatorRestoreDrillResult{}, err
	}
	if err := validateRestoreDrillHeaders(response.header, replayed); err != nil {
		return store.OperatorRestoreDrillResult{}, err
	}
	var result store.OperatorRestoreDrillResult
	if err := decodeStrictJSONDocument(response.body, "restore drill create", &result); err != nil {
		return result, err
	}
	if result.Replayed != replayed || (replayed != (response.status == http.StatusOK)) {
		return result, errors.New("operator client: restore drill replay evidence mismatch")
	}
	if result.Operation.PreviewDigest != body.PreviewDigest || result.Operation.Backup.Name == "" ||
		body.Confirm != store.RestoreDrillConfirmation(result.Operation.Backup.Name) {
		return result, errors.New("operator client: restore drill create does not match request")
	}
	if err := validateRestoreDrillOperation(result.Operation); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) RestoreDrillOperation(ctx context.Context, id string) (store.RestoreDrillOperation, error) {
	if !validRestoreDrillID(id) {
		return store.RestoreDrillOperation{}, errors.New("operator client: invalid restore drill operation id")
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/maintenance/restore-drills/"+url.PathEscape(id), nil)
	if err != nil {
		return store.RestoreDrillOperation{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return store.RestoreDrillOperation{}, err
	}
	if response.status != http.StatusOK {
		return store.RestoreDrillOperation{}, fmt.Errorf("operator client: restore drill detail returned HTTP %d", response.status)
	}
	if err := validateRestoreDrillHeaders(response.header, false); err != nil {
		return store.RestoreDrillOperation{}, err
	}
	var result store.RestoreDrillOperation
	if err := decodeStrictJSONDocument(response.body, "restore drill detail", &result); err != nil {
		return result, err
	}
	if result.OperationID != id {
		return result, errors.New("operator client: restore drill detail identity mismatch")
	}
	if err := validateRestoreDrillOperation(result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) RestoreDrillOperations(ctx context.Context, limit int) (store.RestoreDrillListResult, error) {
	if limit < 0 || limit > store.MaxRestoreDrillReadLimit {
		return store.RestoreDrillListResult{}, errors.New("operator client: invalid restore drill operation limit")
	}
	path := "/v1/operator/maintenance/restore-drills"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return store.RestoreDrillListResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return store.RestoreDrillListResult{}, err
	}
	if response.status != http.StatusOK {
		return store.RestoreDrillListResult{}, fmt.Errorf("operator client: restore drill list returned HTTP %d", response.status)
	}
	if err := validateRestoreDrillHeaders(response.header, false); err != nil {
		return store.RestoreDrillListResult{}, err
	}
	var result store.RestoreDrillListResult
	if err := decodeStrictJSONDocument(response.body, "restore drill list", &result); err != nil {
		return result, err
	}
	if result.SchemaVersion != 1 || result.Consistency != store.RestoreDrillReadConsistency ||
		!canonicalRetentionResponseTime(result.EvaluatedAt) || result.Total < len(result.Items) || result.Items == nil {
		return result, errors.New("operator client: invalid restore drill list envelope")
	}
	for index, operation := range result.Items {
		if err := validateRestoreDrillOperation(operation); err != nil {
			return result, err
		}
		if index > 0 {
			previous := result.Items[index-1]
			if previous.CreatedAt.Before(operation.CreatedAt) ||
				(previous.CreatedAt.Equal(operation.CreatedAt) && previous.OperationID <= operation.OperationID) {
				return result, errors.New("operator client: restore drill list order is invalid")
			}
		}
	}
	return result, nil
}

func validateRestoreDrillHeaders(header http.Header, replayAllowed bool) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return err
	}
	if replayed && !replayAllowed {
		return errors.New("operator client: restore drill read cannot be replayed")
	}
	return nil
}

func validateRestoreDrillPreview(result operator.RestoreDrillPreview) error {
	if result.SchemaVersion != 1 || !canonicalRetentionResponseTime(result.EvaluatedAt) ||
		result.LiveExpected < -1 || !validRestoreDrillBackup(result.Backup) ||
		result.Confirmation != store.RestoreDrillConfirmation(result.Backup.Name) || !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: invalid restore drill preview")
	}
	raw, _ := json.Marshal(struct {
		SchemaVersion int                      `json:"schema_version"`
		Backup        store.RestoreDrillBackup `json:"backup"`
		LiveExpected  int                      `json:"live_expected"`
		Confirmation  string                   `json:"confirmation"`
	}{result.SchemaVersion, result.Backup, result.LiveExpected, result.Confirmation})
	hash := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(hash[:]) != result.PreviewDigest {
		return errors.New("operator client: restore drill preview digest mismatch")
	}
	return nil
}

func validateRestoreDrillOperation(op store.RestoreDrillOperation) error {
	if !validRestoreDrillID(op.OperationID) || !validRestoreDrillBackup(op.Backup) ||
		!validSHA256Digest(op.PreviewDigest) || op.LiveExpectedAtPreview < -1 || op.Attempt < 0 ||
		!canonicalRetentionResponseTime(op.CreatedAt) || !canonicalRetentionResponseTime(op.UpdatedAt) ||
		op.UpdatedAt.Before(op.CreatedAt) {
		return errors.New("operator client: invalid restore drill operation identity")
	}
	switch op.State {
	case store.RestoreDrillQueued:
		if op.Phase != store.RestoreDrillPhaseQueued || op.Attempt != 0 || op.StartedAt != nil || op.FinishedAt != nil ||
			op.Machines != nil || op.Expected != nil || op.LiveExpected != nil || op.NewestCheckinAt != nil ||
			op.DurationMilliseconds != nil || op.ErrorCode != nil || op.ErrorDetail != nil {
			return errors.New("operator client: contradictory queued restore drill")
		}
	case store.RestoreDrillRunning:
		if op.Phase != store.RestoreDrillPhaseVerifying || op.Attempt < 1 || op.StartedAt == nil || op.FinishedAt != nil ||
			op.Machines != nil || op.Expected != nil || op.LiveExpected != nil || op.NewestCheckinAt != nil ||
			op.DurationMilliseconds != nil || op.ErrorCode != nil || op.ErrorDetail != nil {
			return errors.New("operator client: contradictory running restore drill")
		}
	case store.RestoreDrillSucceeded:
		if op.Phase != store.RestoreDrillPhaseComplete || op.StartedAt == nil || op.FinishedAt == nil ||
			op.Machines == nil || *op.Machines < 1 || op.Expected == nil || *op.Expected < 0 || *op.Expected > *op.Machines ||
			op.LiveExpected == nil || *op.LiveExpected < -1 || op.DurationMilliseconds == nil || *op.DurationMilliseconds < 0 ||
			op.ErrorCode != nil || op.ErrorDetail != nil {
			return errors.New("operator client: contradictory succeeded restore drill")
		}
	case store.RestoreDrillFailed:
		if op.Phase != store.RestoreDrillPhaseVerifying || op.StartedAt == nil || op.FinishedAt == nil || op.Machines != nil ||
			op.Expected != nil || op.LiveExpected != nil || op.NewestCheckinAt != nil || op.DurationMilliseconds != nil ||
			op.ErrorCode == nil || *op.ErrorCode == "" || op.ErrorDetail == nil || *op.ErrorDetail == "" {
			return errors.New("operator client: contradictory failed restore drill")
		}
	default:
		return errors.New("operator client: unknown restore drill state")
	}
	for _, value := range []*time.Time{op.StartedAt, op.FinishedAt} {
		if value != nil && !canonicalRetentionResponseTime(*value) {
			return errors.New("operator client: invalid restore drill transition time")
		}
	}
	if op.StartedAt != nil && (op.StartedAt.Before(op.CreatedAt) || op.StartedAt.After(op.UpdatedAt)) {
		return errors.New("operator client: restore drill started time is out of order")
	}
	if op.FinishedAt != nil && (op.StartedAt == nil || op.FinishedAt.Before(*op.StartedAt) || !op.FinishedAt.Equal(op.UpdatedAt)) {
		return errors.New("operator client: restore drill finished time is out of order")
	}
	if op.NewestCheckinAt != nil {
		_, offset := op.NewestCheckinAt.Zone()
		if op.NewestCheckinAt.IsZero() || offset != 0 {
			return errors.New("operator client: invalid restored check-in time")
		}
	}
	return nil
}

func validRestoreDrillBackup(backup store.RestoreDrillBackup) bool {
	_, offset := backup.ModifiedAt.Zone()
	if len(backup.Name) < 1 || len(backup.Name) > 255 || !utf8.ValidString(backup.Name) || filepath.Base(backup.Name) != backup.Name ||
		!strings.HasPrefix(backup.Name, "clawctl-") || !strings.HasSuffix(backup.Name, ".sqlite") ||
		backup.SizeBytes <= 0 || backup.ModifiedAt.IsZero() || offset != 0 || !validSHA256Digest(backup.SHA256) {
		return false
	}
	for _, r := range backup.Name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

func validRestoreDrillID(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || value != strings.TrimSpace(value) ||
		strings.ContainsAny(value, "/\\") || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}
