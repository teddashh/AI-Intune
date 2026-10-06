package operatorclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type assignedUserTransport func(*http.Request) (*http.Response, error)

func (f assignedUserTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func assignedUserTestClient(t *testing.T, respond func(*http.Request) (MachineAssignedUserResponse, string)) *Client {
	t.Helper()
	base, _ := url.Parse("http://100.64.0.1:8080")
	return &Client{base: base, http: &http.Client{Transport: assignedUserTransport(func(r *http.Request) (*http.Response, error) {
		result, etag := respond(r)
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		h := http.Header{"Content-Type": []string{"application/json"}, "Cache-Control": []string{"no-store"}, "Etag": []string{etag}}
		if result.Replayed {
			h.Set("Idempotency-Replayed", "true")
		}
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
	})}}
}

func TestAssignedUserClientReadWriteReplayAndClear(t *testing.T) {
	for _, user := range []string{"42", "none"} {
		for _, replay := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%t", user, replay), func(t *testing.T) {
				client := assignedUserTestClient(t, func(r *http.Request) (MachineAssignedUserResponse, string) {
					if r.URL.Path != "/v1/operator/machines/machine-1/assigned-user" {
						t.Fatal(r.URL)
					}
					result := MachineAssignedUserResponse{MachineID: "machine-1", DisplayName: "機器"}
					if r.Method == http.MethodPut {
						var body MachineAssignedUserRequest
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						if body.UserID != user || body.ConfirmDisplayName != "機器" || body.ExpectedRevision != 0 || r.Header.Get("Idempotency-Key") != "request-key" {
							t.Fatalf("要求=%+v %v", body, r.Header)
						}
						if user == "42" {
							result.UserID = "42"
							result.UserLogin = "user@example.com"
							result.Revision = 1
						}
						result.Replayed = replay
					}
					return result, fmt.Sprintf(`"assigned-user-revision-%d"`, result.Revision)
				})
				got, err := client.GetMachineAssignedUser(t.Context(), "machine-1")
				if err != nil || got.Meta.ETag != `"assigned-user-revision-0"` {
					t.Fatalf("讀取=%+v %v", got, err)
				}
				got, err = client.PutMachineAssignedUser(t.Context(), "machine-1", "request-key", MachineAssignedUserRequest{UserID: user, ConfirmDisplayName: "機器"})
				if err != nil || got.Replayed != replay {
					t.Fatalf("指派=%+v %v", got, err)
				}
			})
		}
	}
}

func TestAssignedUserClientRejectsContradictoryResponses(t *testing.T) {
	for _, mode := range []string{"錯誤標籤", "錯誤目標", "錯誤版本", "錯誤使用者", "控制字元", "讀取重放"} {
		t.Run(mode, func(t *testing.T) {
			client := assignedUserTestClient(t, func(r *http.Request) (MachineAssignedUserResponse, string) {
				result := MachineAssignedUserResponse{MachineID: "machine-1", DisplayName: "機器", UserID: "42", UserLogin: "user@example.com", Revision: 1}
				etag := `"assigned-user-revision-1"`
				switch mode {
				case "錯誤標籤":
					etag = `"channel-revision-1"`
				case "錯誤目標":
					result.MachineID = "other"
				case "錯誤版本":
					result.Revision = 2
					etag = `"assigned-user-revision-2"`
				case "錯誤使用者":
					result.UserID = "99"
				case "控制字元":
					result.UserLogin = "user\x1b@example.com"
				case "讀取重放":
					result.Replayed = true
					result.PreviousUserID = "42"
					result.PreviousUserLogin = result.UserLogin
				}
				return result, etag
			})
			var err error
			if mode == "讀取重放" {
				_, err = client.GetMachineAssignedUser(t.Context(), "machine-1")
			} else {
				_, err = client.PutMachineAssignedUser(t.Context(), "machine-1", "key", MachineAssignedUserRequest{UserID: "42", ConfirmDisplayName: "機器"})
			}
			if err == nil {
				t.Fatal("接受了不符的回應")
			}
		})
	}
}
