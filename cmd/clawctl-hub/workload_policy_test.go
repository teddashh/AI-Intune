package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestCheckinHandsOffPolicyTokenBoundToRulesAndCurrentDisplayName(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rule := model.Expectation{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof",
		MaxAgeSeconds: 3600, Why: "policy handoff test",
	}
	exps := &expect.Set{Configured: true, Rules: []model.Expectation{rule}}
	st.SetExpectations(exps)
	if err := st.PublishExpectationsPolicy(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	h := &hub{store: st, expects: exps}
	mux := http.NewServeMux()
	h.machineAndPublicRoutes(mux)
	machine := enrollViaHTTP(t, mux, st, "samplehub1")

	checkin := func() model.CheckinResponse {
		t.Helper()
		raw, _ := json.Marshal(model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: time.Now().UTC(),
			AgentVersion: "test", BootID: "boot", AgentSeq: 1,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/checkins", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+machine.token)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("checkin=%d: %s", rec.Code, rec.Body.String())
		}
		var resp model.CheckinResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	first := checkin()
	want, err := st.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkloadPolicyToken != want || want == "" || len(first.Expectations) != 1 {
		t.Fatalf("checkin did not bind rules/token: token=%q want=%q rules=%+v", first.WorkloadPolicyToken, want, first.Expectations)
	}

	if _, err := st.DB().Exec(`UPDATE machine_registry SET display_name='cnode-renamed' WHERE machine_id=?`, machine.id); err != nil {
		t.Fatal(err)
	}
	second := checkin()
	wantRenamed, err := st.CurrentWorkloadPolicyToken("cnode-renamed")
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkloadPolicyToken != wantRenamed || wantRenamed == want {
		t.Fatalf("rename did not rotate policy token: before=%q after=%q want=%q", want, second.WorkloadPolicyToken, wantRenamed)
	}
	if len(second.Expectations) != 0 {
		t.Fatalf("old-name machine-specific rule survived rename: %+v", second.Expectations)
	}
}
