package artifact

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestPolicyVersionForSourceKind(t *testing.T) {
	want := []struct {
		kind   string
		policy string
	}{
		{ArtifactSourceNPM, FetchPolicyVersion},
		{ArtifactSourceNode, NodeRuntimeFetchPolicyVersion},
		{ArtifactSourceHermesImage, HermesImageFetchPolicyVersion},
		{ArtifactSourceClaudeCode, ClaudeCodeFetchPolicyVersion},
		{ArtifactSourceCodex, CodexFetchPolicyVersion},
		{ArtifactSourceGrok, GrokFetchPolicyVersion},
		{ArtifactSourceBATServer, BATServerFetchPolicyVersion},
		{ArtifactSourceAntigravity, AntigravityFetchPolicyVersion},
	}
	seen := make(map[string]string, len(want))
	for _, item := range want {
		got, err := PolicyVersionForSourceKind(item.kind)
		if err != nil || got != item.policy {
			t.Fatalf("kind %q policy=%q err=%v want %q", item.kind, got, err, item.policy)
		}
		if _, ok := seen[item.kind]; ok {
			t.Fatalf("duplicate source kind %q", item.kind)
		}
		seen[item.kind] = item.policy
	}
	for _, kind := range []string{"", "unknown-source", FetchPolicyVersion} {
		got, err := PolicyVersionForSourceKind(kind)
		if err == nil || got != "" || !errors.Is(err, ErrInvalidFetchRequest) {
			t.Fatalf("kind %q policy=%q err=%v", kind, got, err)
		}
	}

	declared := declaredArtifactSourceKinds(t)
	if len(declared) != len(want) {
		t.Fatalf("declared source kinds=%d table=%d (%v)", len(declared), len(want), declared)
	}
	for name, kind := range declared {
		policy, ok := seen[kind]
		if !ok {
			t.Fatalf("constant %s=%q is missing from the policy table", name, kind)
		}
		got, err := PolicyVersionForSourceKind(kind)
		if err != nil || got != policy {
			t.Fatalf("constant %s policy=%q err=%v want %q", name, got, err, policy)
		}
	}
}

func declaredArtifactSourceKinds(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values := spec.(*ast.ValueSpec)
				for i, ident := range values.Names {
					if !strings.HasPrefix(ident.Name, "ArtifactSource") {
						continue
					}
					if i >= len(values.Values) {
						t.Fatalf("%s is not a string literal", ident.Name)
					}
					literal, ok := values.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Fatalf("%s is not a string literal", ident.Name)
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					found[ident.Name] = value
				}
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no ArtifactSource constants declared")
	}
	return found
}
