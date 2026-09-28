package operatorclient

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestVerifierRegisterRejectsACredentialTheOperatorCannotUse(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	credential := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	request := VerifierCreateRequest{
		Kind:          store.VerifierKindFleetPeerAgent,
		DisplayName:   "fleet-peer-verifier",
		FailureDomain: "fleet-peer-domain",
		PreviewDigest: digest,
		Reason:        "register verifier",
	}
	honest := VerifierResponse{
		VerifierID:       "verifier-1",
		Kind:             request.Kind,
		DisplayName:      request.DisplayName,
		FailureDomain:    request.FailureDomain,
		CreatedAt:        time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Revision:         1,
		PreviewDigest:    request.PreviewDigest,
		Credential:       credential,
		SecretAvailable:  true,
		Replayed:         false,
		RecoveryRequired: false,
		RecoveryAction:   "",
	}
	register := func(result VerifierResponse) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.RegisterVerifier(
			t.Context(), "cli-verifier-register-key", request,
		)
		return err
	}
	if err := register(honest); err != nil {
		t.Fatalf("honest registration was rejected, so the cases below would not "+
			"measure the credential-shape clause under test: %v", err)
	}

	tests := []struct {
		name        string
		credential  string
		consequence string
	}{
		{
			name:       "credential uses the standard base64 alphabet",
			credential: "+" + credential[1:],
			consequence: "the CLI would print a standard-base64 value as the " +
				"one-time secret, every POST /v1/verifications would fail " +
				"authentication, and the operator's only recovery would be to " +
				"revoke and register again",
		},
		{
			name:       "credential is not a 32-byte secret",
			credential: credential[:20],
			consequence: "the CLI would print a truncated value as the one-time " +
				"secret, every POST /v1/verifications would fail authentication, " +
				"and the operator's only recovery would be to revoke and register " +
				"again",
		},
	}
	const expected = "operator client: fresh verifier response does not contain one valid available credential"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := honest
			result.Credential = tc.credential
			err := register(result)
			if err == nil || err.Error() != expected {
				var got string
				if err != nil {
					got = err.Error()
				}
				t.Errorf("got error %q, expected %q; %s", got, expected, tc.consequence)
			}
		})
	}
}

// validateVerifierRevocationImpact 是 operator 這一側唯一會質疑 Hub 交回來的
// 撤銷影響數字的地方。store 端那兩個數字在撤銷之後無法重算（live 重算會得到
// 0），所以 CLI 拿到什麼就印什麼；Web 沒有 verifier revoke 畫面，這條 CLI
// 路徑就是 operator 唯一的第二意見。這支測試之前，整支 validate 函式焊成
// return nil，operatorclient 與 cmd/clawctl-hub 的測試全部仍然綠。
// ⚠ 這裡刻意沒有打 `evidenceRows < 0`：它永遠不會是唯一開火的那一項
// （jobs >= 0 時 `jobs > evidenceRows` 先成立，jobs < 0 時 `jobs < 0` 先成立），
// 補了也只是在量隔壁那一項。
func TestVerifierRevocationRejectsAnImpactTheHubCouldNotHaveMeasured(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	revision := int64(3)
	request := VerifierRevocationRequest{
		ExpectedRevision:   &revision,
		ConfirmDisplayName: "fleet-peer-verifier",
		PreviewDigest:      digest,
		Reason:             "rotating the peer credential",
	}
	honest := store.OperatorVerifierRevocationResult{
		VerifierID:             "verifier-1",
		Kind:                   store.VerifierKindFleetPeerAgent,
		DisplayName:            request.ConfirmDisplayName,
		FailureDomain:          "fleet-peer-domain",
		RevokedAt:              time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		PreviousRevision:       revision,
		Revision:               revision + 1,
		EvidenceRows:           2,
		JobsLosingOnlyProducer: 1,
		RegistryRowRetained:    true,
		EvidenceRetained:       true,
		PreviewDigest:          digest,
		Replayed:               false,
	}
	revoke := func(result store.OperatorVerifierRevocationResult) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.RevokeVerifier(
			t.Context(), "verifier-1", "cli-verifier-revoke-key", request,
		)
		return err
	}
	if err := revoke(honest); err != nil {
		t.Fatalf("honest revocation was rejected, so the cases below would not "+
			"measure the revocation-impact clause under test: %v", err)
	}

	tests := []struct {
		name        string
		mutate      func(*store.OperatorVerifierRevocationResult)
		expected    string
		consequence string
	}{
		{
			name: "more jobs lose their producer than there is evidence",
			mutate: func(r *store.OperatorVerifierRevocationResult) {
				r.JobsLosingOnlyProducer = 3
			},
			expected: "operator client: verifier revocation impact counts are inconsistent",
			consequence: "the CLI would print an impact line claiming more jobs lose their " +
				"only independent producer than there are independent evidence rows, and the " +
				"operator would schedule re-verification for jobs the Hub never measured",
		},
		{
			name: "the count of jobs losing their producer is negative",
			mutate: func(r *store.OperatorVerifierRevocationResult) {
				r.EvidenceRows = 0
				r.JobsLosingOnlyProducer = -1
			},
			expected: "operator client: verifier revocation impact counts are inconsistent",
			consequence: "the CLI would print a negative count of jobs losing their only " +
				"independent producer, and the operator could not tell an impact the Hub failed " +
				"to measure from a measured zero",
		},
		{
			name: "the hub says the evidence is not retained",
			mutate: func(r *store.OperatorVerifierRevocationResult) {
				r.EvidenceRetained = false
			},
			expected: "operator client: verifier revocation would drop the row or its evidence",
			consequence: "the CLI would report the revocation as complete while the Hub said " +
				"the independent verification evidence was dropped, so the operator would never " +
				"learn that the evidence behind past job verdicts is gone",
		},
		{
			name: "the hub says the registry row is not retained",
			mutate: func(r *store.OperatorVerifierRevocationResult) {
				r.RegistryRowRetained = false
			},
			expected: "operator client: verifier revocation would drop the row or its evidence",
			consequence: "the CLI would report the revocation as complete while the Hub said " +
				"the registry row was dropped, so the operator would keep looking for a revoked " +
				"row that the Hub said no longer exists",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := honest
			tc.mutate(&result)
			err := revoke(result)
			if err == nil || err.Error() != tc.expected {
				var got string
				if err != nil {
					got = err.Error()
				}
				t.Errorf("got error %q, expected %q; %s", got, tc.expected, tc.consequence)
			}
		})
	}
}
