package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestDailyReportCLIDiscoversTheRunningHubWithoutTouchingSQLite(t *testing.T) {
	f, _ := reportFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, grantedOperatorRequest(r, true, true))
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	var discoveries atomic.Int32
	deps.discoverHubURL = func() (string, error) {
		discoveries.Add(1)
		return base, nil
	}
	deps.validateDirectPath = func(string) error {
		t.Fatal("正常 report 預覽碰了 direct DB")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	var out, errOut bytes.Buffer
	if err := runDailyReportCommandWithDeps(t.Context(), []string{"--since", "1h"}, &out, &errOut, deps); err != nil {
		t.Fatalf("report: %v; stderr=%s", err, errOut.String())
	}
	if discoveries.Load() != 1 || !strings.Contains(out.String(), "cnode-operator") ||
		!strings.Contains(out.String(), "http://100.64.200.2:8787/") {
		t.Fatalf("discoveries=%d output=%q", discoveries.Load(), out.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("正常 report 預覽建立了 fallback DB：%v", err)
	}
}

func TestDailyReportCLIDiscoveryFailureNeverFallsBackToSQLite(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
		newOperatorClient: func(string) (*operatorclient.Client, error) {
			t.Fatal("discovery failure constructed an HTTP client")
			return nil, nil
		},
		validateDirectPath: func(string) error {
			t.Fatal("discovery failure inspected direct DB")
			return nil
		},
		openDirectDB: func(string) (*store.Store, error) {
			t.Fatal("discovery failure opened direct DB")
			return nil, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runDailyReportCommandWithDeps(t.Context(), nil, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery failure established fallback DB：%v", err)
	}
}

func TestDailyReportCLIExplicitDBUsesTheStoppedServiceFence(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	publishMachineReadTestPolicy(t, dbPath)

	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("明示 --db 做了 Hub discovery")
		return "", nil
	}
	deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		t.Fatal("明示 --db 建立了 HTTP client")
		return nil, nil
	}
	var stopped atomic.Int32
	deps.verifyHubStopped = func(_ context.Context, got string) error {
		if got != dbPath {
			t.Fatalf("stopped proof path=%q want=%q", got, dbPath)
		}
		stopped.Add(1)
		return nil
	}
	var out, errOut bytes.Buffer
	if err := runDailyReportCommandWithDeps(t.Context(), []string{
		"--db", dbPath, "--since", "1h", "--listen", "100.64.200.2:8787",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("direct report: %v; stderr=%s", err, errOut.String())
	}
	if stopped.Load() != 1 || !strings.Contains(out.String(), "/1 報到") ||
		!strings.Contains(out.String(), "http://100.64.200.2:8787/") {
		t.Fatalf("stopped=%d output=%q", stopped.Load(), out.String())
	}
}

func TestDailyReportCLIExplicitDBRejectsDifferentWorkloadPolicy(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	publishMachineReadTestPolicy(t, dbPath)
	policyPath := filepath.Join(t.TempDir(), "different-expectations.json")
	if err := os.WriteFile(policyPath, []byte(`{"expectations":[{"machine":"*","unit":"private-unit","artifact":"/private/sentinel","why":"different policy","max_age_seconds":60}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWCTL_EXPECTATIONS", policyPath)
	deps := productionMachineCommandDeps()
	deps.verifyHubStopped = func(context.Context, string) error { return nil }
	var out, errOut bytes.Buffer
	err := runDailyReportCommandWithDeps(t.Context(), []string{"--db", dbPath}, &out, &errOut, deps)
	if err == nil || !errors.Is(err, store.ErrWorkloadPolicyIdentityMismatch) || out.Len() != 0 {
		t.Fatalf("policy mismatch error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func TestDailyReportCLIRejectsInvalidInputBeforeChoosingATransport(t *testing.T) {
	for name, argv := range map[string][]string{
		"兩個 transport":       {"--hub-url", "http://100.64.0.1:8787", "--db", "/tmp/hub.db"},
		"HTTP mode 的 listen": {"--listen", "100.64.0.1:8787"},
		"positional":         {"daily"},
		"重複 since":           {"--since", "1h", "--since", "2h"},
		"零 window":           {"--since", "0s"},
		"subsecond":          {"--since", "1500ms"},
		"超過上限":               {"--since", "721h"},
		"空 db":               {"--db", ""},
		"控制字元":               {"--hub-url", "http://100.64.0.1:8787\x00"},
	} {
		t.Run(name, func(t *testing.T) {
			deps := machineCommandDeps{
				discoverHubURL: func() (string, error) {
					t.Fatal("不合法輸入做了 discovery")
					return "", nil
				},
				newOperatorClient: func(string) (*operatorclient.Client, error) {
					t.Fatal("不合法輸入建立了 HTTP client")
					return nil, nil
				},
				validateDirectPath: func(string) error {
					t.Fatal("不合法輸入檢查了 direct DB")
					return nil
				},
			}
			var out, errOut bytes.Buffer
			if err := runDailyReportCommandWithDeps(t.Context(), argv, &out, &errOut, deps); err == nil {
				t.Fatalf("被接受了：output=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

// report 的正常路徑只准建立 operator client。明示 --db 的相容路徑也必須只把 DB
// 交給共用 fence；舊的 mustOpen/openExisting 一旦被放回這支指令，就重新長出第二 writer。
func TestDailyReportCommandHasNoUnfencedStoreOpen(t *testing.T) {
	source := readRepoFile(t, "dailyreportcmd.go")
	for _, forbidden := range []string{"mustOpen(", "openExisting(", "store.Open("} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("daily report command regained unfenced SQLite open %q", forbidden)
		}
	}
	for _, required := range []string{"reportHTTPClient(", "withDirectOperatorStore("} {
		if !strings.Contains(source, required) {
			t.Fatalf("daily report command lost transport boundary %q", required)
		}
	}
}
