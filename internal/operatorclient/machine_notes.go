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
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/store"
)

type MachineNotesPreviewRequest struct {
	Notes string `json:"notes"`
}

type MachineNotesPreviewResponse = store.OperatorMachineNotesPreviewResult

type MachineNotesRequest struct {
	Notes              string `json:"notes"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
}

type MachineNotesResponse = store.OperatorMachineNotesResult

func (c *Client) PreviewMachineNotes(ctx context.Context, machineID string,
	body MachineNotesPreviewRequest,
) (MachineNotesPreviewResponse, error) {
	var out MachineNotesPreviewResponse
	if err := validateNotesText(body.Notes); err != nil {
		return out, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, fmt.Errorf("operator client: encode machine notes preview: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPost, machineID,
		"/notes-preview", bytes.NewReader(raw))
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
	if err := decodeStrictJSONDocument(response.body, "machine notes preview", &out); err != nil {
		return out, err
	}
	if out.MachineID != machineID || out.DisplayName == "" || out.Notes != body.Notes ||
		out.CurrentNotes == out.Notes || out.PreviewedAt.IsZero() || out.PreviewedAt.Location() != time.UTC ||
		!out.RegistryNotesChanged || !out.MachineConfigurationUnchanged || !out.AgentUnaffected ||
		!validSHA256Digest(out.PreviewDigest) {
		return MachineNotesPreviewResponse{}, errors.New("operator client: machine notes preview does not conform to target and effect contract")
	}
	if err := validateNotesText(out.CurrentNotes); err != nil {
		return MachineNotesPreviewResponse{}, err
	}
	if err := validateRenameText(out.DisplayName); err != nil {
		return MachineNotesPreviewResponse{}, err
	}
	return out, nil
}

func (c *Client) PutMachineNotes(ctx context.Context, machineID, idempotencyKey string,
	body MachineNotesRequest,
) (MachineNotesResponse, error) {
	var out MachineNotesResponse
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200 ||
		validateNotesText(body.Notes) != nil ||
		validateRenameText(body.ConfirmDisplayName) != nil ||
		!validSHA256Digest(body.PreviewDigest) || strings.TrimSpace(body.Reason) == "" ||
		body.Reason != strings.TrimSpace(body.Reason) || len(body.Reason) > 500 {
		return out, errors.New("operator client: machine notes apply request is incomplete or invalid")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, fmt.Errorf("operator client: encode machine notes apply: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPut, machineID,
		"/notes", bytes.NewReader(raw))
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
	if err := decodeStrictJSONDocument(response.body, "machine notes apply", &out); err != nil {
		return out, err
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return MachineNotesResponse{}, err
	}
	if out.MachineID != machineID || out.DisplayName != body.ConfirmDisplayName ||
		out.NotesPresent != (body.Notes != "") || out.AppliedAt.IsZero() || out.AppliedAt.Location() != time.UTC ||
		!out.RegistryNotesChanged || !out.MachineConfigurationUnchanged || !out.AgentUnaffected ||
		out.PreviewDigest != body.PreviewDigest || out.Replayed != replayed {
		return MachineNotesResponse{}, errors.New("operator client: machine notes result does not conform to request and effect contract")
	}
	return out, nil
}

func validateNotesText(value string) error {
	if len(value) > store.OperatorMachineNotesMaxBytes || !utf8.ValidString(value) {
		return errors.New("operator client: machine notes is invalid")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("operator client: machine notes cannot have leading or trailing whitespace")
	}
	if value != "" {
		if err := validateMachineClientText("machine notes", value, store.OperatorMachineNotesMaxBytes); err != nil {
			return err
		}
	}
	return nil
}
