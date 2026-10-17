package operatorclient

import (
	"context"
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/certificates"
	"net/http"
	"testing"
)

func TestCertificateClient(t *testing.T) {
	client := settingClientServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/operator/certificates":
			w.Write([]byte(`{"items":[]}`))
		case "/v1/operator/certificates/monitor-1":
			json.NewEncoder(w).Encode(certificates.Status{Monitor: certificates.Monitor{ID: "monitor-1", Config: certificates.Config{Expected: []string{}, Days: []int{}}, Result: certificates.Result{SANs: []string{}, Gaps: []string{}}}, Flags: []string{"stale probe"}})
		default:
			http.NotFound(w, r)
		}
	})
	for _, id := range []string{"", "monitor-1"} {
		if _, err := client.CertificateRead(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.CertificateRead(context.Background(), "absent"); err == nil {
		t.Fatal("missing read succeeded")
	}
}
