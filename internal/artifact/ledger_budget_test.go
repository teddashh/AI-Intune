package artifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestPreviewBudgetsFitDurableArtifactFetchLedger(t *testing.T) {
	t.Run("schema", func(t *testing.T) {
		limit, err := artifactFetchOperationsMaxBytesLimit(readArtifactFetchSchema(t))
		if err != nil {
			t.Fatal(err)
		}
		declared := declaredArtifactSourceKinds(t)
		if len(artifactSourcePreviewBudgets) != len(declared) {
			t.Fatalf("budget table=%d declared kinds=%d (%v)", len(artifactSourcePreviewBudgets), len(declared), declared)
		}
		seen := make(map[string]string, len(declared))
		fits, exceeds := 0, 0
		for name, kind := range declared {
			if previous, ok := seen[kind]; ok {
				t.Fatalf("constants %s and %s share source kind %q", previous, name, kind)
			}
			seen[kind] = name
			maxBytes, ledger, err := previewBudgetForSourceKind(kind)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			switch ledger {
			case durableLedgerFits:
				fits++
				if maxBytes > limit {
					t.Fatalf("%s preview max bytes %d exceeds artifact_fetch_operations max_bytes limit %d", name, maxBytes, limit)
				}
			case durableLedgerExceeds:
				exceeds++
				if maxBytes <= limit {
					t.Fatalf("%s is marked outside the durable ledger but preview max bytes %d fits limit %d", name, maxBytes, limit)
				}
			default:
				t.Fatalf("%s has no durable ledger disposition", name)
			}
		}
		if fits == 0 || exceeds == 0 {
			t.Fatalf("fits=%d exceeds=%d", fits, exceeds)
		}
		if _, _, err := previewBudgetForSourceKind(""); err == nil || !errors.Is(err, ErrInvalidFetchRequest) {
			t.Fatalf("empty kind err=%v", err)
		}
	})

	t.Run("reads the check literal", func(t *testing.T) {
		const schema = `
CREATE TABLE IF NOT EXISTS artifact_fetch_operations (
  max_bytes        INTEGER NOT NULL CHECK (max_bytes > 0 AND max_bytes <= 7),
);`
		got, err := artifactFetchOperationsMaxBytesLimit(schema)
		if err != nil || got != 7 {
			t.Fatalf("limit=%d err=%v", got, err)
		}
	})

	t.Run("unparsed check fails", func(t *testing.T) {
		samples := []string{
			"",
			"CREATE TABLE IF NOT EXISTS artifact_fetch_operations (name TEXT);",
			`CREATE TABLE IF NOT EXISTS artifact_fetch_operations (
  max_bytes INTEGER NOT NULL CHECK (max_bytes > 0 AND max_bytes <= max_bytes),
);`,
			`CREATE TABLE IF NOT EXISTS other_operations (
  max_bytes INTEGER NOT NULL CHECK (max_bytes > 0 AND max_bytes <= 7),
);`,
			`CREATE TABLE IF NOT EXISTS artifact_fetch_operations (
  max_bytes INTEGER NOT NULL CHECK (max_bytes > 0 AND max_bytes <= 7),
);
CREATE TABLE IF NOT EXISTS artifact_fetch_operations (
  max_bytes INTEGER NOT NULL CHECK (max_bytes > 0 AND max_bytes <= 9),
);`,
		}
		for _, schema := range samples {
			got, err := artifactFetchOperationsMaxBytesLimit(schema)
			if err == nil || got != 0 {
				t.Fatalf("schema %q limit=%d err=%v", schema, got, err)
			}
		}
	})
}

func readArtifactFetchSchema(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate schema reader")
	}
	path := filepath.Join(filepath.Dir(file), "..", "store", "schema.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("schema.sql is empty")
	}
	return string(raw)
}

var artifactFetchMaxBytesCheck = regexp.MustCompile(
	`(?m)^\s*max_bytes\s+INTEGER\s+NOT NULL\s+CHECK\s*\(\s*max_bytes\s*>\s*0\s+AND\s+max_bytes\s*<=\s*([0-9]+)\s*\)`,
)

// artifactFetchOperationsMaxBytesLimit reads the max_bytes upper bound from the
// artifact_fetch_operations column CHECK. A missing or ambiguous CHECK is an error.
func artifactFetchOperationsMaxBytesLimit(schema string) (int64, error) {
	const marker = "CREATE TABLE IF NOT EXISTS artifact_fetch_operations"
	if strings.Count(schema, marker) != 1 {
		return 0, errors.New("artifact_fetch_operations table was not parsed")
	}
	start := strings.Index(schema, marker)
	open := strings.Index(schema[start:], "(")
	if open < 0 {
		return 0, errors.New("artifact_fetch_operations table has no column list")
	}
	body, err := sqlParenBody(schema, start+open)
	if err != nil {
		return 0, err
	}
	matches := artifactFetchMaxBytesCheck.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return 0, fmt.Errorf("artifact_fetch_operations max_bytes CHECK matched %d times", len(matches))
	}
	limit, err := strconv.ParseInt(matches[0][1], 10, 64)
	if err != nil || limit <= 0 {
		return 0, errors.New("artifact_fetch_operations max_bytes CHECK limit was not parsed")
	}
	return limit, nil
}

func sqlParenBody(schema string, open int) (string, error) {
	if open < 0 || open >= len(schema) || schema[open] != '(' {
		return "", errors.New("artifact_fetch_operations column list was not parsed")
	}
	depth := 0
	for i := open; i < len(schema); i++ {
		switch schema[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return schema[open+1 : i], nil
			}
		}
	}
	return "", errors.New("artifact_fetch_operations column list was not parsed")
}
