package operatorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type MachineAssignedUserRequest struct {
	UserID             string `json:"user_id"`
	ExpectedRevision   int64  `json:"expected_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
}

type MachineAssignedUserResponse struct {
	MachineID         string           `json:"machine_id"`
	DisplayName       string           `json:"display_name"`
	PreviousUserID    string           `json:"previous_user_id"`
	PreviousUserLogin string           `json:"previous_user_login"`
	UserID            string           `json:"user_id"`
	UserLogin         string           `json:"user_login"`
	Revision          int64            `json:"revision"`
	Replayed          bool             `json:"replayed"`
	Meta              ResponseMetadata `json:"-"`
}

func (c *Client) GetMachineAssignedUser(ctx context.Context, machineID string) (MachineAssignedUserResponse, error) {
	return c.machineAssignedUser(ctx, machineID, "", nil)
}

func (c *Client) PutMachineAssignedUser(ctx context.Context, machineID, key string, body MachineAssignedUserRequest) (MachineAssignedUserResponse, error) {
	if strings.TrimSpace(key) == "" {
		return MachineAssignedUserResponse{}, errors.New("Idempotency-Key 不可省略")
	}
	return c.machineAssignedUser(ctx, machineID, key, &body)
}

func (c *Client) machineAssignedUser(ctx context.Context, machineID, key string, body *MachineAssignedUserRequest) (MachineAssignedUserResponse, error) {
	var out MachineAssignedUserResponse
	method := http.MethodGet
	var raw []byte
	if body != nil {
		method = http.MethodPut
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return out, err
		}
	}
	req, err := c.newMachineOperatorRequest(ctx, method, machineID, "/assigned-user", bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("指派使用者回應狀態無效：HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "指派使用者", &out); err != nil {
		return MachineAssignedUserResponse{}, err
	}
	etags := response.header.Values("ETag")
	if out.Revision < 0 || len(etags) != 1 || etags[0] != fmt.Sprintf(`"assigned-user-revision-%d"`, out.Revision) {
		return MachineAssignedUserResponse{}, errors.New("指派使用者版本不符")
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return MachineAssignedUserResponse{}, err
	}
	if out.MachineID != machineID || out.DisplayName == "" || out.Replayed != replayed ||
		!validAssignedUserPair(out.UserID, out.UserLogin) || !validAssignedUserPair(out.PreviousUserID, out.PreviousUserLogin) {
		return MachineAssignedUserResponse{}, errors.New("指派使用者回應內容不符")
	}
	for _, value := range []string{out.DisplayName, out.UserLogin, out.PreviousUserLogin} {
		if !utf8.ValidString(value) || len(value) > 4096 || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
			return MachineAssignedUserResponse{}, errors.New("指派使用者文字無效")
		}
	}
	if body == nil {
		if out.Replayed || out.PreviousUserID != out.UserID || out.PreviousUserLogin != out.UserLogin {
			return MachineAssignedUserResponse{}, errors.New("讀取指派使用者回應不符")
		}
	} else {
		wanted := body.UserID
		if wanted == "none" {
			wanted = ""
		}
		revision := body.ExpectedRevision
		if out.PreviousUserID != out.UserID || out.PreviousUserLogin != out.UserLogin {
			revision++
		}
		if body.UserID == "" || out.UserID != wanted || out.DisplayName != body.ConfirmDisplayName || out.Revision != revision {
			return MachineAssignedUserResponse{}, errors.New("指派使用者回應與要求不符")
		}
	}
	out.Meta = ResponseMetadata{ETag: etags[0], ETagRevision: out.Revision, IdempotencyReplayed: replayed}
	return out, nil
}

func validAssignedUserPair(id, login string) bool {
	if id == "" {
		return login == ""
	}
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id && strings.TrimSpace(login) != "" && len(login) <= 320
}
