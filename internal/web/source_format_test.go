package web

import (
	"bytes"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ⚠ make test 不跑 fmt，所以格式要由 go test ./... 這道真的會跑的門來守。
func TestEveryGoSourceFileIsGofmtClean(t *testing.T) {
	roots := []string{"../../cmd", ".."}
	checked := 0
	var failures []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(path, ".go") {
				return nil
			}

			checked++
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			formatted, err := format.Source(raw)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v (fix with: gofmt -w %s)", path, err, path))
				return nil
			}
			if !bytes.Equal(raw, formatted) {
				failures = append(failures, fmt.Sprintf("%s (fix with: gofmt -w %s)", path, path))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked == 0 {
		t.Fatal("checked zero Go source files")
	}
	if len(failures) > 0 {
		t.Errorf("Go source files are not gofmt-clean:\n%s", strings.Join(failures, "\n"))
	}
}
