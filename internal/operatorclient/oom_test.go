package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestOOMClientReadAndValidation(t *testing.T) {
	now := time.Now().UTC()
	result := operator.OOMKillResult{SchemaVersion: 1, EvaluatedAt: now, Items: []model.OOMKill{{Time: now, Source: "kernel", Process: "worker"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/operator/machines/example/oom-kills" || r.URL.RawQuery != "" {
			t.Error("incorrect request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	c := operatorClientForServer(t, server)
	if out, err := c.MachineOOMKills(t.Context(), "example"); err != nil || len(out.Items) != 1 {
		t.Fatalf("read %v", err)
	}
	result.Items[0].Process = "worker password=private"
	if _, err := c.MachineOOMKills(t.Context(), "example"); err == nil {
		t.Fatal("accepted free text")
	}
	result.Items = nil
	if _, err := c.MachineOOMKills(t.Context(), "example"); err == nil {
		t.Fatal("accepted missing array")
	}
	for _, id := range []string{"", "..", "a/b"} {
		if _, err := c.MachineOOMKills(t.Context(), id); err == nil {
			t.Fatal("accepted malformed ID")
		}
	}
}
