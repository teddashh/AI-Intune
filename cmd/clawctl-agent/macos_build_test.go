package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// macOS 是目標平台（docs/PRODUCT.md 5.5），這支測試守的是「agent 還編得起來」
// 這個前置閘門；它不驗行為，行為要等 macOS 上真的跑起來才量得到。
func TestAgentStillCompilesForMacOS(t *testing.T) {
	goPath := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goPath); err != nil {
		var lookPathErr error
		goPath, lookPathErr = exec.LookPath("go")
		if lookPathErr != nil {
			t.Skip("找不到 Go toolchain，略過 macOS agent 編譯前置閘門。")
		}
	}

	for _, arch := range []string{"arm64", "amd64"} {
		arch := arch
		t.Run(arch, func(t *testing.T) {
			cmd := exec.Command(goPath, "build", "-o", os.DevNull, ".")
			cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+arch)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("無法為 macOS/%s 編譯 clawctl-agent；macOS 是目標平台，agent 編不起來就沒有 macOS 報到。編譯器輸出：\n%s", arch, output)
			}
		})
	}
}
