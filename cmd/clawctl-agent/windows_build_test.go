package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAgentStillCompilesForWindows(t *testing.T) {
	goPath := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goPath); err != nil {
		var lookPathErr error
		goPath, lookPathErr = exec.LookPath("go")
		if lookPathErr != nil {
			t.Skip("找不到 Go toolchain，略過 Windows agent 編譯前置閘門。")
		}
	}

	for _, arch := range []string{"amd64", "arm64"} {
		arch := arch
		t.Run(arch, func(t *testing.T) {
			cmd := exec.Command(goPath, "build", "-o", os.DevNull, ".")
			cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+arch, "CGO_ENABLED=0")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("無法為 windows/%s 編譯 clawctl-agent。編譯器輸出：\n%s", arch, output)
			}
		})
	}
}
