package operatorclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceTokenFileAndOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	secret := "cst_" + strings.Repeat("a", 64)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewWithTokenFile("https://hub.example.com", path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.newOperatorRequest(context.Background(), "GET", "/v1/operator/machines", nil)
	if err != nil || r.Header.Get("Authorization") != "Bearer "+secret {
		t.Fatal("missing bearer")
	}
	if _, err = c.newOperatorRequest(context.Background(), "GET", "/account/security", nil); err == nil {
		t.Fatal("non-operator accepted")
	}
	for _, mode := range []os.FileMode{0644, 0400, 0700} {
		if err = os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err = ReadServiceTokenFile(path); err == nil {
			t.Fatal("bad mode accepted")
		}
	}
	_ = os.Chmod(path, 0600)
	link := path + "-link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadServiceTokenFile(link); err == nil {
		t.Fatal("symlink accepted")
	}
	for _, origin := range []string{"http://hub.example.com", "https://user:password@hub.example.com", "https://hub.example.com/path", "https://hub.example.com?x=1", "https://hub.example.com#fragment"} {
		if _, err = ServiceOrigin(origin); err == nil {
			t.Fatal("invalid origin accepted")
		}
	}
}

func TestServiceTokenHTTPSRequest(t *testing.T) {
	secret := "cst_" + strings.Repeat("a", 64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret || r.URL.Path != "/v1/operator/machines" {
			t.Error("incorrect authenticated request")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := NewWithTokenFile(server.URL, path)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport, err = transportWithoutAmbientProxy(server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.newOperatorRequest(context.Background(), "GET", "/v1/operator/machines", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.doRaw(request)
	if err != nil || response.status != http.StatusNoContent {
		t.Fatal("HTTPS request failed")
	}
}
