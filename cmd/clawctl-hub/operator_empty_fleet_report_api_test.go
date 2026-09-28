package main

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorReportsSurviveTheirOwnClientOnAnEmptyFleet(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("開啟空機隊測試資料庫失敗：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	(&hub{
		store: st, artifactsDir: artifactsDirFor(dbPath), retention: store.DefaultRetention(),
	}).operatorRoutes(mux)
	client, _ := operatorClientForMuxWithoutListener(t, mux, "http://100.64.0.9:8787")

	t.Run("安裝報告", func(t *testing.T) {
		install, err := client.InstallReport(t.Context())
		if err != nil {
			t.Fatalf("空機隊安裝報告的 client 錯誤：%v", err)
		}
		if len(install.Resources) != 0 || install.Headline == "" {
			t.Fatalf("空機隊安裝報告 resources 長度=%d，headline=%q", len(install.Resources), install.Headline)
		}
	})

	t.Run("軟體報告", func(t *testing.T) {
		software, err := client.SoftwareReport(t.Context())
		if err != nil {
			t.Fatalf("空機隊軟體報告的 client 錯誤：%v", err)
		}
		if len(software.Tools) != 0 || software.Headline == "" {
			t.Fatalf("空機隊軟體報告 tools 長度=%d，headline=%q", len(software.Tools), software.Headline)
		}
	})

	t.Run("註冊報告", func(t *testing.T) {
		if _, err := client.EnrollmentReport(t.Context()); err != nil {
			t.Fatalf("空機隊註冊報告的 client 錯誤：%v", err)
		}
	})

	t.Run("profile 對照報告", func(t *testing.T) {
		if _, err := client.ProfileReport(t.Context()); err != nil {
			t.Fatalf("空機隊 profile 報告的 client 錯誤：%v", err)
		}
	})
}
