package agentadapter

import (
	"reflect"
	"testing"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

func TestRegistryReturnsDefensiveDeterministicCopies(t *testing.T) {
	contracts := Contracts()
	if len(contracts) != 8 || contracts[0].Adapter.Name != "node-runtime" ||
		contracts[0].Adapter.Version != 1 || contracts[0].PackageID != "node-runtime" ||
		contracts[0].PackageKind != appcatalog.KindRuntime ||
		contracts[0].ExecutorKind != ExecutorKindNodeRuntime || !contracts[0].RequiresMeasuredEvidence ||
		len(contracts[0].Platforms) != 6 ||
		contracts[1].ExecutorKind != ExecutorKindOpenClaw || contracts[1].RequiresMeasuredEvidence ||
		contracts[2].ExecutorKind != ExecutorKindHermes || contracts[2].RequiresMeasuredEvidence ||
		contracts[3].ExecutorKind != ExecutorKindClaudeCode || !contracts[3].RequiresMeasuredEvidence ||
		len(contracts[3].Platforms) != 6 ||
		contracts[4].ExecutorKind != ExecutorKindCodex || contracts[4].PackageKind != appcatalog.KindApp ||
		!contracts[4].RequiresMeasuredEvidence || len(contracts[4].Platforms) != 6 ||
		contracts[5].ExecutorKind != ExecutorKindGrok || contracts[5].PackageID != "grok" ||
		contracts[5].PackageKind != appcatalog.KindApp || !contracts[5].RequiresMeasuredEvidence ||
		len(contracts[5].Platforms) != 6 ||
		contracts[6].ExecutorKind != ExecutorKindBATServer || contracts[6].PackageID != "bat-server" ||
		contracts[6].PackageKind != appcatalog.KindApp || !contracts[6].RequiresMeasuredEvidence ||
		len(contracts[6].Platforms) != 2 ||
		contracts[6].Platforms[0] != (appcatalog.Platform{OS: "linux", Arch: "amd64"}) ||
		contracts[6].Platforms[1] != (appcatalog.Platform{OS: "linux", Arch: "arm64"}) ||
		contracts[7].ExecutorKind != ExecutorKindAntigravity || contracts[7].PackageID != "antigravity" ||
		contracts[7].PackageKind != appcatalog.KindApp || !contracts[7].RequiresMeasuredEvidence ||
		len(contracts[7].Platforms) != 6 || contracts[7].ProbeTool != "agy" {
		t.Fatalf("contracts=%+v", contracts)
	}
	contracts[0].Platforms[0].OS = "changed"
	again := Contracts()
	if again[0].Platforms[0].OS != "linux" ||
		!reflect.DeepEqual(ExecutorKinds(), []string{"antigravity", "bat-server", "claude-code", "codex", "grok", "hermes-agent", "node-runtime", "openclaw"}) {
		t.Fatalf("registry mutated=%+v kinds=%v", again, ExecutorKinds())
	}
}

func TestSupportsManifestRequiresExactContract(t *testing.T) {
	manifest := appcatalog.Manifest{
		ID: "openclaw", Kind: appcatalog.KindApp,
		Adapter: appcatalog.Adapter{Name: "openclaw", Version: 1},
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
		},
	}
	if !SupportsManifest(manifest) {
		t.Fatal("exact OpenClaw contract is unsupported")
	}
	for _, mutate := range []func(*appcatalog.Manifest){
		func(m *appcatalog.Manifest) { m.ID = "another" },
		func(m *appcatalog.Manifest) { m.Kind = appcatalog.KindRuntime },
		func(m *appcatalog.Manifest) { m.Adapter.Version = 2 },
		func(m *appcatalog.Manifest) { m.Platforms = []appcatalog.Platform{{OS: "darwin", Arch: "arm64"}} },
	} {
		candidate := manifest
		mutate(&candidate)
		if SupportsManifest(candidate) {
			t.Fatalf("unsupported contract accepted: %+v", candidate)
		}
	}
	node := manifest
	node.ID = "node-runtime"
	node.Kind = appcatalog.KindRuntime
	node.Adapter.Name = "node-runtime"
	if !SupportsManifest(node) {
		t.Fatal("exact Node runtime contract is unsupported")
	}
	hermes := manifest
	hermes.ID = "hermes-agent"
	hermes.Adapter.Name = "hermes-agent"
	if !SupportsManifest(hermes) {
		t.Fatal("exact Hermes contract is unsupported")
	}
	if !HasExecutorForPlatform(appcatalog.Platform{OS: "linux", Arch: "amd64"}) ||
		!HasExecutorForPlatform(appcatalog.Platform{OS: "darwin", Arch: "amd64"}) ||
		!HasExecutorForPlatform(appcatalog.Platform{OS: "darwin", Arch: "arm64"}) ||
		!HasExecutorForPlatform(appcatalog.Platform{OS: "windows", Arch: "amd64"}) ||
		!HasExecutorForPlatform(appcatalog.Platform{OS: "windows", Arch: "arm64"}) {
		t.Fatal("platform support does not match the registered executor contracts")
	}
	node.Platforms = []appcatalog.Platform{{OS: "darwin", Arch: "arm64"}}
	if !SupportsManifest(node) {
		t.Fatal("exact Darwin Node runtime contract is unsupported")
	}
	node.Platforms = []appcatalog.Platform{{OS: "windows", Arch: "amd64"}}
	if !SupportsManifest(node) {
		t.Fatal("exact Windows Node runtime contract is unsupported")
	}
}

func TestPackageForProbeToolRefusesAmbiguousDeclarations(t *testing.T) {
	declared := map[string]string{}
	for _, contract := range registered {
		if contract.ProbeTool == "" {
			continue
		}
		if other, ok := declared[contract.ProbeTool]; ok {
			t.Fatalf("probe tool %q is declared by %s and %s", contract.ProbeTool, other, contract.PackageID)
		}
		declared[contract.ProbeTool] = contract.PackageID
	}
	original := registered
	t.Cleanup(func() { registered = original })
	registered = append(append([]Contract(nil), original...), Contract{PackageID: "agy-shadow", ProbeTool: "agy"})
	if got, ok := PackageForProbeTool("agy"); ok {
		t.Fatalf("ambiguous probe tool resolved to %q", got)
	}
}

func TestPackageForProbeToolMapsDeclaredCommandsOnly(t *testing.T) {
	mapped := map[string]string{
		"openclaw": "openclaw", "claude": "claude-code", "codex": "codex", "grok": "grok", "agy": "antigravity",
	}
	for tool, packageID := range mapped {
		got, ok := PackageForProbeTool(tool)
		if !ok || got != packageID {
			t.Fatalf("%s -> %q ok=%t", tool, got, ok)
		}
	}
	for _, tool := range []string{"", "gemini", "legacy-tool", "hermes-1.4.2", "node-runtime", "hermes-agent", "bat-server"} {
		if got, ok := PackageForProbeTool(tool); ok {
			t.Fatalf("unmapped %q resolved to %q", tool, got)
		}
	}
}
