package agentadapter

import (
	"reflect"
	"testing"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

func TestRegistryReturnsDefensiveDeterministicCopies(t *testing.T) {
	contracts := Contracts()
	if len(contracts) != 3 || contracts[0].Adapter.Name != "node-runtime" ||
		contracts[0].Adapter.Version != 1 || contracts[0].PackageID != "node-runtime" ||
		contracts[0].PackageKind != appcatalog.KindRuntime ||
		contracts[0].ExecutorKind != ExecutorKindNodeRuntime || len(contracts[0].Platforms) != 4 ||
		contracts[1].ExecutorKind != ExecutorKindOpenClaw || contracts[2].ExecutorKind != ExecutorKindHermes {
		t.Fatalf("contracts=%+v", contracts)
	}
	contracts[0].Platforms[0].OS = "changed"
	again := Contracts()
	if again[0].Platforms[0].OS != "linux" ||
		!reflect.DeepEqual(ExecutorKinds(), []string{"hermes-agent", "node-runtime", "openclaw"}) {
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
		!HasExecutorForPlatform(appcatalog.Platform{OS: "darwin", Arch: "arm64"}) {
		t.Fatal("platform support does not match the registered executor contracts")
	}
	node.Platforms = []appcatalog.Platform{{OS: "darwin", Arch: "arm64"}}
	if !SupportsManifest(node) {
		t.Fatal("exact Darwin Node runtime contract is unsupported")
	}
}
