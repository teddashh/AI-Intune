package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBootstrapBundlesCarryTheTargetInstallerAndService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bootstrap bundles are built on the Linux release host with GNU tar")
	}
	root := t.TempDir()
	for _, dir := range []string{"ops", "build"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"build-agent-bundles.sh", "install-agent.sh", "install-agent-macos.sh",
		"clawctl-agent.service", "clawctl-hermes.service", "openclaw-gateway.service", "clawctl-agent.plist"} {
		if err := os.WriteFile(filepath.Join(root, "ops", name), []byte(readOpsFile(t, name)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), makefile, 0o600); err != nil {
		t.Fatal(err)
	}
	const version = "bootstrap-test"
	for _, target := range []string{"darwin", "linux"} {
		t.Run(target, func(t *testing.T) {
			for _, arch := range []string{"amd64", "arm64"} {
				name := "clawctl-agent-" + target + "-" + arch
				if err := os.WriteFile(filepath.Join(root, "build", name), []byte(name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{filepath.Join(root, "ops", "build-agent-bundles.sh"), version}
			if target == "darwin" {
				args = append(args, target)
			}
			run := func() {
				t.Helper()
				if out, err := exec.Command("bash", args...).CombinedOutput(); err != nil {
					t.Fatalf("bundle builder: %v\n%s", err, out)
				}
			}
			run()
			first := map[string][]byte{}
			for _, arch := range []string{"amd64", "arm64"} {
				name := "clawctl-agent-bootstrap-" + target + "-" + arch + ".tar.gz"
				body, err := os.ReadFile(filepath.Join(root, "build", name))
				if err != nil {
					t.Fatal(err)
				}
				first[name] = body
				want := map[string]string{"VERSION": version + "\n", "clawctl-agent": "clawctl-agent-" + target + "-" + arch}
				support := []string{"install-agent.sh", "clawctl-agent.service", "clawctl-hermes.service", "openclaw-gateway.service"}
				if target == "darwin" {
					support = []string{"install-agent-macos.sh", "clawctl-agent.plist"}
				}
				for _, file := range support {
					want[file] = readOpsFile(t, file)
				}
				gz, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				tr := tar.NewReader(gz)
				for {
					header, err := tr.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					expected, ok := want[header.Name]
					mode := int64(0o644)
					if header.Name == "clawctl-agent" || strings.HasPrefix(header.Name, "install-agent") {
						mode = 0o755
					}
					content, err := io.ReadAll(tr)
					if !ok || err != nil || string(content) != expected || header.Typeflag != tar.TypeReg || header.Mode != mode {
						t.Fatalf("%s entry %q: content/layout/mode mismatch (mode=%o, err=%v)", name, header.Name, header.Mode, err)
					}
					delete(want, header.Name)
				}
				if len(want) != 0 {
					t.Fatalf("%s is missing %d required files", name, len(want))
				}
			}
			run()
			for name, original := range first {
				body, err := os.ReadFile(filepath.Join(root, "build", name))
				if err != nil || !bytes.Equal(body, original) {
					t.Fatalf("%s is not reproducible: %v", name, err)
				}
			}
		})
	}
	cmd := exec.Command("make", "-n", "agent-bundles-darwin", "VERSION="+version, "GO=go")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "GOOS=darwin GOARCH=amd64") ||
		!strings.Contains(string(out), "GOOS=darwin GOARCH=arm64") ||
		!strings.Contains(string(out), `./ops/build-agent-bundles.sh "`+version+`" darwin`) {
		t.Fatalf("Mac bundle target must build both binaries before packaging: %v\n%s", err, out)
	}
}
