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

	"github.com/teddashh/AI-Intune/internal/store"
)

type MachineRenamePreviewRequest struct {
	DisplayName string `json:"display_name"`
}

type MachineRenamePreviewResponse = store.OperatorMachineRenamePreviewResult

type MachineRenameRequest struct {
	DisplayName        string `json:"display_name"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
}

type MachineRenameResponse = store.OperatorMachineRenameResult

func (c *Client) PreviewMachineRename(ctx context.Context, machineID string,
	body MachineRenamePreviewRequest,
) (MachineRenamePreviewResponse, error) {
	var out MachineRenamePreviewResponse
	if err := validateRenameText(body.DisplayName); err != nil {
		return out, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, fmt.Errorf("operator client: encode machine rename preview: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPost, machineID,
		"/display-name-preview", bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "machine rename preview", &out); err != nil {
		return out, err
	}
	if out.MachineID != machineID || out.DisplayName != body.DisplayName ||
		out.CurrentDisplayName == "" || out.CurrentDisplayName == out.DisplayName ||
		out.PreviewedAt.IsZero() || out.PreviewedAt.Location() != time.UTC ||
		!out.MachineIDPreserved || !out.AgentUnaffected || !out.ExpectationKeyChanges ||
		!validSHA256Digest(out.PreviewDigest) {
		return MachineRenamePreviewResponse{}, errors.New("operator client: machine rename preview 不符合 target 與影響契約")
	}
	if err := validateRenameText(out.CurrentDisplayName); err != nil {
		return MachineRenamePreviewResponse{}, err
	}
	return out, nil
}

func (c *Client) PutMachineRename(ctx context.Context, machineID, idempotencyKey string,
	body MachineRenameRequest,
) (MachineRenameResponse, error) {
	var out MachineRenameResponse
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200 ||
		validateRenameText(body.DisplayName) != nil || validateRenameText(body.ConfirmDisplayName) != nil ||
		!validSHA256Digest(body.PreviewDigest) || strings.TrimSpace(body.Reason) == "" ||
		body.Reason != strings.TrimSpace(body.Reason) || len(body.Reason) > 500 {
		return out, errors.New("operator client: machine rename apply request 不完整或不合法")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, fmt.Errorf("operator client: encode machine rename apply: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPut, machineID,
		"/display-name", bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "machine rename apply", &out); err != nil {
		return out, err
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return MachineRenameResponse{}, err
	}
	if out.MachineID != machineID || out.PreviousDisplayName != body.ConfirmDisplayName ||
		out.DisplayName != body.DisplayName || out.AppliedAt.IsZero() || out.AppliedAt.Location() != time.UTC ||
		!out.MachineIDPreserved || !out.AgentUnaffected || !out.ExpectationKeyChanged ||
		out.PreviewDigest != body.PreviewDigest ||
		out.Replayed != replayed {
		return MachineRenameResponse{}, errors.New("operator client: machine rename result 不符合 request 與影響契約")
	}
	return out, nil
}

func validateRenameText(value string) error {
	if err := validateMachineClientText("machine display_name", value,
		store.OperatorMachineDisplayNameMaxBytes); err != nil {
		return err
	}
	if value != strings.TrimSpace(value) {
		return errors.New("operator client: machine display_name 前後不可有空白")
	}
	return nil
}
