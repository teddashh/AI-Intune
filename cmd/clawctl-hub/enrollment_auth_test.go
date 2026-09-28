package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Enrollment is the authority hand-off that enables the agent's deployment
// loop. Missing, wrong, or expired one-time credentials must all stop before a
// long-lived machine bearer exists, and machine routes must reject anything
// except the bearer from a complete receipt.
func TestEnrollmentCredentialGatesManagedDeployment(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mux := http.NewServeMux()
	(&hub{store: st}).machineAndPublicRoutes(mux)

	expiredID, expired, err := st.CreateEnrollTokenFor("expired", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE enrollment_tokens SET expires_at=? WHERE used_by=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), expiredID); err != nil {
		t.Fatal(err)
	}

	requestEnroll := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token,
			Hostname: "host", UnixUser: "user", OS: "linux", Arch: "amd64",
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/enrollments", bytes.NewReader(raw)))
		return rec
	}

	var rejectionBody string
	for name, token := range map[string]string{
		"missing": "",
		"wrong":   "not-an-enrollment-token",
		"expired": expired,
	} {
		t.Run(name, func(t *testing.T) {
			rec := requestEnroll(token)
			assertAPIError(t, rec, http.StatusForbidden, model.ErrEnrollTokenBad)
			if rejectionBody == "" {
				rejectionBody = rec.Body.String()
			} else if rec.Body.String() != rejectionBody {
				t.Fatalf("credential rejection leaked which token failed: %s", rec.Body.String())
			}
		})
	}
	var expiredHash, expiredUsed any
	if err := st.DB().QueryRow(`SELECT m.agent_token_hash,e.used_at
		FROM machine_registry m JOIN enrollment_tokens e ON e.used_by=m.machine_id
		WHERE m.machine_id=?`, expiredID).Scan(&expiredHash, &expiredUsed); err != nil {
		t.Fatal(err)
	}
	if expiredHash != nil || expiredUsed != nil {
		t.Fatalf("expired ticket created authority: agent_token_hash=%v used_at=%v", expiredHash, expiredUsed)
	}

	_, fresh, err := st.CreateEnrollTokenFor("deploy-ready", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	success := requestEnroll(fresh)
	if success.Code != http.StatusOK {
		t.Fatalf("valid enrollment=%d: %s", success.Code, success.Body.String())
	}
	var receipt model.EnrollResponse
	if err := json.Unmarshal(success.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.SchemaVersion != model.SchemaVersion || receipt.MachineID == "" || receipt.AgentToken == "" ||
		receipt.CheckinIntervalSeconds <= 0 || receipt.ObservationIntervalSeconds <= 0 || receipt.SettingsDigest == "" {
		t.Fatalf("incomplete authority receipt: %+v", receipt)
	}

	requestNext := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/jobs/next", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := requestNext(receipt.AgentToken); rec.Code != http.StatusNoContent {
		t.Fatalf("receipt credential cannot reach deploy queue: %d %s", rec.Code, rec.Body.String())
	}
	missing := requestNext("")
	wrong := requestNext("wrong-agent-token")
	assertAPIError(t, missing, http.StatusUnauthorized, model.ErrUnauthorized)
	assertAPIError(t, wrong, http.StatusUnauthorized, model.ErrUnauthorized)
}
