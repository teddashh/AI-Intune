package operatorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// EnrollmentLimitRequest 是送出去改上限的那一份。
//
// ⚠ Set 與 MaxMachines 分開送，因為「不設上限」跟「上限 0」是相反的兩個意思，
// 一個用 0 代表無限的欄位沒辦法同時表達它們。
type EnrollmentLimitRequest struct {
	Set              bool   `json:"set"`
	MaxMachines      int    `json:"max_machines"`
	ExpectedRevision int64  `json:"expected_revision"`
	PreviewDigest    string `json:"preview_digest"`
	Reason           string `json:"reason"`
}

// EnrollmentLimit reads how many more machines this Hub will take.
func (c *Client) EnrollmentLimit(ctx context.Context) (operator.EnrollmentLimitResult, error) {
	var out operator.EnrollmentLimitResult
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/enrollment-limit", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: enrollment limit returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "enrollment limit", &out); err != nil {
		return out, err
	}
	if err := validateEnrollmentLimitResult(out); err != nil {
		return out, err
	}
	return out, nil
}

// PreviewEnrollmentLimit asks what changing the limit would do.
func (c *Client) PreviewEnrollmentLimit(ctx context.Context,
	body operator.EnrollmentLimitPreviewRequest,
) (operator.EnrollmentLimitPreview, error) {
	var out operator.EnrollmentLimitPreview
	if err := validateEnrollmentLimitIntent(body.Set, body.MaxMachines); err != nil {
		return out, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, fmt.Errorf("operator client: encode enrollment limit preview request: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/enrollment-limit/preview", bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: enrollment limit preview returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "enrollment limit preview", &out); err != nil {
		return out, err
	}
	if err := validateEnrollmentLimitPreview(out, body); err != nil {
		return out, err
	}
	return out, nil
}

// SetEnrollmentLimit changes the limit.
func (c *Client) SetEnrollmentLimit(ctx context.Context, idempotencyKey string,
	body EnrollmentLimitRequest,
) (store.OperatorEnrollmentLimitResult, error) {
	var out store.OperatorEnrollmentLimitResult
	if err := validateEnrollmentLimitApplyRequest(idempotencyKey, body); err != nil {
		return out, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, fmt.Errorf("operator client: encode enrollment limit request: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/enrollment-limit", bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: enrollment limit apply returned HTTP %d", response.status)
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return out, err
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "enrollment limit apply", &out); err != nil {
		return out, err
	}
	if out.Replayed != replayed {
		return out, fmt.Errorf("operator client: enrollment limit replay evidence mismatch body=%t header=%t",
			out.Replayed, replayed)
	}
	if err := validateEnrollmentLimitApplyResult(out, body); err != nil {
		return out, err
	}
	return out, nil
}

func validateEnrollmentLimitIntent(set bool, maxMachines int) error {
	if !set {
		if maxMachines != 0 {
			return errors.New("operator client: machine count cannot be provided when clearing limit")
		}
		return nil
	}
	if maxMachines < 0 || maxMachines > operator.MaxEnrollmentLimitMachines {
		return fmt.Errorf("operator client: limit must be between 0 and %d", operator.MaxEnrollmentLimitMachines)
	}
	return nil
}

func validateEnrollmentLimitApplyRequest(idempotencyKey string, body EnrollmentLimitRequest) error {
	if err := validateEnrollmentLimitIntent(body.Set, body.MaxMachines); err != nil {
		return err
	}
	if idempotencyKey == "" || validateDeploymentClientText("Idempotency-Key", idempotencyKey, 200, false) != nil {
		return errors.New("operator client: Idempotency-Key is required, cannot contain leading/trailing whitespace or control characters, and cannot exceed 200 bytes")
	}
	if body.PreviewDigest == "" {
		return errors.New("operator client: preview_digest is required")
	}
	if body.ExpectedRevision < 0 {
		return errors.New("operator client: expected_revision cannot be negative")
	}
	// ⚠ 首尾空白要自己擋。validateDeploymentClientText 不看它，而 Hub 收下之後會
	// TrimSpace——結果是 Web 與 CLI 拒絕的同一份理由，走這條路會被默默改寫。稽核上
	// 留下來的理由必須就是操作的人打的那一份。
	if strings.TrimSpace(body.Reason) != body.Reason ||
		validateDeploymentClientText("reason", body.Reason, 500, false) != nil {
		return errors.New("operator client: reason is required, cannot contain leading/trailing whitespace or control characters, and cannot exceed 500 bytes")
	}
	return nil
}

// validateEnrollmentLimitState 自己把「還可以再納管幾台」算一次。
//
// ⚠ headroom 跟 at_limit 是這一頁上唯一會讓人改變行為的兩個欄位：一個說得出
// 「還收得下」的回應，如果它的兩個數字其實已經到頂，會讓操作的人一直重試一個
// 永遠不會成功的動作，而畫面上沒有任何地方寫著為什麼。
func validateEnrollmentLimitState(state store.EnrollmentLimitState) error {
	if state.InDenominator < 0 || state.Retired < 0 || state.Headroom < 0 || state.Revision < 0 {
		return errors.New("operator client: enrollment limit has negative values")
	}
	if !state.Set {
		// 沒有上限的時候台數沒有意義，而 at_limit 一定是 false：這個 Hub 收得下
		// 下一台。一份帶著舊台數的「沒有上限」會在畫面上讀成「上限就是那個數字」。
		if state.MaxMachines != 0 || state.AtLimit || state.Headroom != 0 {
			return errors.New("operator client: no limit configured, but carries limit details")
		}
		// revision 0 是「從來沒設定過」，那一種沒有人改過，也就沒有理由可以講。
		if state.Revision == 0 && (state.Reason != "" || state.UpdatedBy != "" || !state.UpdatedAt.IsZero()) {
			return errors.New("operator client: limit was never configured, but specifies who changed it")
		}
		if state.Revision > 0 && (state.UpdatedBy == "" || state.UpdatedAt.IsZero()) {
			return errors.New("operator client: limit was modified, but does not specify who changed it")
		}
		if state.Revision > 0 && state.UpdatedAt.Location() != time.UTC {
			return errors.New("operator client: enrollment limit updated_at is inconsistent")
		}
		return nil
	}
	if state.Revision < 1 {
		return errors.New("operator client: limit is configured but lacks revision")
	}
	if state.UpdatedAt.IsZero() || state.UpdatedAt.Location() != time.UTC {
		return errors.New("operator client: enrollment limit updated_at is inconsistent")
	}
	headroom := state.MaxMachines - state.InDenominator
	if headroom < 0 {
		headroom = 0
	}
	if state.Headroom != headroom {
		return fmt.Errorf("operator client: limit is %d machines, enrolled %d machines, headroom should be %d machines, reported %d machines",
			state.MaxMachines, state.InDenominator, headroom, state.Headroom)
	}
	if state.AtLimit != (state.InDenominator >= state.MaxMachines) {
		return fmt.Errorf("operator client: limit is %d machines, enrolled %d machines, at_limit reports %t",
			state.MaxMachines, state.InDenominator, state.AtLimit)
	}
	return nil
}

func validateEnrollmentLimitResult(result operator.EnrollmentLimitResult) error {
	if result.SchemaVersion != operator.EnrollmentLimitSchemaVersion ||
		result.GeneratedAt.IsZero() || result.GeneratedAt.Location() != time.UTC {
		return errors.New("operator client: enrollment limit identity is inconsistent")
	}
	if err := validateEnrollmentLimitState(result.State); err != nil {
		return err
	}
	if result.Headline == "" {
		return errors.New("operator client: enrollment limit does not indicate current state")
	}
	// ⚠ 開不了票卻沒說下一步做什麼，等於把人留在一個沒有出口的畫面上。
	if result.State.Set && result.State.AtLimit && result.NextStep == "" {
		return errors.New("operator client: limit reached but lacks next step")
	}
	if (!result.State.Set || !result.State.AtLimit) && result.NextStep != "" {
		return errors.New("operator client: not blocked but suggests next action")
	}
	return nil
}

func validateEnrollmentLimitPreview(preview operator.EnrollmentLimitPreview,
	request operator.EnrollmentLimitPreviewRequest,
) error {
	if preview.Set != request.Set || (request.Set && preview.MaxMachines != request.MaxMachines) {
		return errors.New("operator client: enrollment limit preview returned does not match requested limit")
	}
	if err := validateEnrollmentLimitState(preview.Current); err != nil {
		return err
	}
	if preview.PreviewDigest == "" || preview.Headline == "" ||
		preview.PreviewedAt.IsZero() || preview.PreviewedAt.Location() != time.UTC {
		return errors.New("operator client: enrollment limit preview identity is inconsistent")
	}
	if preview.ExpectedRevision != preview.Current.Revision {
		return fmt.Errorf("operator client: preview expected revision %d, server saw %d",
			preview.ExpectedRevision, preview.Current.Revision)
	}
	// ⚠ 「改完就已經超過」要自己再判一次。它是確認頁上唯一一句會讓人改主意的話：
	// 這個上限不會退役任何一台，但下一張票就開不出來了。
	if alreadyOver := preview.Set && preview.Current.InDenominator > preview.MaxMachines; preview.AlreadyOver != alreadyOver {
		return fmt.Errorf("operator client: limit changing to %d machines, enrolled %d machines, already_over reports %t",
			preview.MaxMachines, preview.Current.InDenominator, preview.AlreadyOver)
	}
	return nil
}

func validateEnrollmentLimitApplyResult(result store.OperatorEnrollmentLimitResult,
	request EnrollmentLimitRequest,
) error {
	if err := validateEnrollmentLimitState(result.State); err != nil {
		return err
	}
	// ⚠ 回放帶回來的是現在的上限，不是當時改成什麼——那是刻意的，因為呼叫端拿這個
	// 結果去畫畫面。所以只有真的寫進去的那一次可以拿請求去對，回放那一次不行。
	if result.Replayed {
		return nil
	}
	if result.State.Set != request.Set || (request.Set && result.State.MaxMachines != request.MaxMachines) {
		return errors.New("operator client: applied limit does not match submitted limit")
	}
	if result.State.Revision <= request.ExpectedRevision {
		return fmt.Errorf("operator client: submitted at revision %d, applied at revision %d",
			request.ExpectedRevision, result.State.Revision)
	}
	return nil
}
