package scrubguard

import (
	"bytes"
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

//go:embed denylist.txt
var denylistText string

// maxScanBytes skips large vendored or generated files; none of them carry
// fleet identifiers, and the scan stays fast.
const maxScanBytes = 4 << 20

func TestDenylistIsWellFormed(t *testing.T) {
	deny, bad := ParseDenylist(denylistText)
	if len(bad) != 0 {
		t.Fatalf("denylist.txt has malformed lines: %q", bad)
	}
	if len(deny) < 20 {
		t.Fatalf("denylist.txt has %d digests; the list was truncated", len(deny))
	}
}

func TestCandidatesCoverPiecesAndDomainSuffixes(t *testing.T) {
	got := strings.Join(Candidates("Node-A.corp.example.com"), " ")
	for _, want := range []string{"node-a.corp.example.com", "node", "a", "node-a", "corp.example.com", "example.com", "com"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("Candidates missing %q in %q", want, got)
		}
	}
	got = strings.Join(Candidates("someone@mail.example.org"), " ")
	for _, want := range []string{"someone", "mail.example.org", "example.org"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("Candidates missing %q in %q", want, got)
		}
	}
}

func TestScanTextFindsAHashedToken(t *testing.T) {
	deny := map[string]struct{}{Digest("forbidden-host"): {}, Digest("example.net"): {}}
	hits := ScanText("ok line\nssh forbidden-host.example.org\nmail a@b.example.net\nfine", deny)
	if len(hits) != 2 || hits[0].Line != 2 || hits[1].Line != 3 {
		t.Fatalf("hits=%+v", hits)
	}
	if hits := ScanText("forbidden hosts are fine", deny); len(hits) != 0 {
		t.Fatalf("partial word matched: %+v", hits)
	}
}

// TestRepositoryHasNoDenylistedTokens is the guard. A failure names the file,
// line, and token; rename the value to a neutral sample (samplehub1,
// sampleagent1, example.com, 100.64.x.y) rather than editing the denylist.
// Paths under overlay/ are a downstream fork's private overlay and are skipped.
func TestRepositoryHasNoDenylistedTokens(t *testing.T) {
	deny, bad := ParseDenylist(denylistText)
	if len(bad) != 0 {
		t.Fatalf("denylist.txt has malformed lines: %q", bad)
	}
	root := repoRoot(t)
	files := trackedFiles(t, root)
	if len(files) < 100 {
		t.Fatalf("found %d files under %s; the scan is not looking at the repository", len(files), root)
	}
	var failures []string
	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "overlay/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted in the work tree but still in the index
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if len(b) > maxScanBytes || bytes.IndexByte(b, 0) >= 0 {
			continue
		}
		for _, h := range ScanText(string(b), deny) {
			failures = append(failures, rel+":"+strconv.Itoa(h.Line)+": "+h.Token)
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d private fleet identifier(s) found; replace them with neutral samples:\n%s",
			len(failures), strings.Join(failures, "\n"))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// trackedFiles prefers git's index so untracked scratch files do not fail the
// guard; without git (a source tarball) it walks the tree.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "-C", root, "ls-files", "-z")
		if out, err := cmd.Output(); err == nil {
			var files []string
			for _, f := range strings.Split(string(out), "\x00") {
				if f != "" {
					files = append(files, f)
				}
			}
			if len(files) > 0 {
				return files
			}
		}
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "build" || d.Name() == "dist") {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
