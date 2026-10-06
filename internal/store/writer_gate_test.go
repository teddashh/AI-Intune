package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The writer pool has exactly one connection. A read or a second Begin on
// s.db while the same goroutine holds a write transaction waits forever (the
// plain database/sql calls have no deadline), which stalls every writer in the
// Hub. Production code must go through beginWrite/execWrite for writes and
// s.rdb for reads. This test keeps new code (for example a branch written
// before the single-writer change) from reintroducing direct s.db use.
func TestProductionStoreCodeDoesNotBypassTheWriterGate(t *testing.T) {
	direct := regexp.MustCompile(`\bs\.db\.(Query|QueryRow|QueryContext|QueryRowContext|Begin|BeginTx|Exec|ExecContext|Prepare|PrepareContext)\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if direct.MatchString(line) {
				t.Errorf("%s:%d uses s.db directly; use s.beginWrite/s.execWrite for writes or s.rdb for reads: %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
