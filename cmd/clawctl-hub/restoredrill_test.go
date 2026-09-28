package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// 造一份「真的」備份：用正式的 store 寫兩台機器、一顆心跳，關掉，抄進備份目錄。
func makeBackup(t *testing.T, dir, name string, now time.Time, machines int) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "live.sqlite")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < machines; i++ {
		tok, err := st.CreateEnrollToken("m", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := st.RedeemEnrollToken(tok, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: tok,
			Hostname: "m", OS: "linux", Arch: "amd64", UnixUser: "example-user",
		}, time.Now()) // ⚠ 票是用真的時鐘開的，換成假的 now 會被當成過期
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO machine_checkins(machine_id, sent_at, received_at) VALUES(?,?,?)`,
			id, now.Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, name)
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// 章：第一行是 unix 秒，讀回來要對得上；沒有章 = 沒做過；壞掉的章也 = 沒做過。
func TestDrillStampRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restore-drill.stamp")
	if _, ok := readDrillStamp(path); ok {
		t.Fatal("沒有章卻說做過")
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := writeDrillStamp(path, now, drillResult{Backup: "/x/clawctl-a.sqlite", Machines: 2, Expected: 2}); err != nil {
		t.Fatal(err)
	}
	at, ok := readDrillStamp(path)
	if !ok || !at.Equal(now) {
		t.Errorf("讀回來不對：%v %v", at, ok)
	}
	os.WriteFile(path, []byte("garbage\n"), 0o644)
	if _, ok := readDrillStamp(path); ok {
		t.Error("壞掉的章要當成沒做過，不能當成剛做過")
	}
}

// /metrics：有章才有線；沒章就沒有這條線（規則用 absent() 接）。
func TestMetricsExposeRestoreDrillOnlyWhenStamped(t *testing.T) {
	mux, st := emptyMetricsFixture(t)
	_ = mux
	h := &hub{store: st, drillStamp: filepath.Join(t.TempDir(), "s")}
	m := http.NewServeMux()
	m.HandleFunc("/metrics", h.handleMetrics)
	if body := getMetrics(t, m); strings.Contains(body, "clawctl_restore_drill_timestamp_seconds") {
		t.Error("沒有章卻吐了線")
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := writeDrillStamp(h.drillStamp, now, drillResult{}); err != nil {
		t.Fatal(err)
	}
	body := getMetrics(t, m)
	if !strings.Contains(body, "clawctl_restore_drill_timestamp_seconds "+"1788696000") {
		t.Errorf("有章卻沒吐對：\n%s", body)
	}
}
