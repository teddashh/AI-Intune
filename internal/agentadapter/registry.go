// Package agentadapter is the single registry joining catalog manifests,
// Hub job resource kinds, and clawctl-agent executor dispatch.
package agentadapter

import (
	"sort"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

const (
	ExecutorKindOpenClaw    = "openclaw"
	ExecutorKindNodeRuntime = "node-runtime"
	ExecutorKindHermes      = "hermes-agent"
)

type Contract struct {
	Adapter      appcatalog.Adapter
	PackageID    string
	PackageKind  appcatalog.PackageKind
	ExecutorKind string
	Platforms    []appcatalog.Platform
}

var registered = []Contract{
	{
		Adapter:   appcatalog.Adapter{Name: "node-runtime", Version: 1},
		PackageID: "node-runtime", PackageKind: appcatalog.KindRuntime,
		ExecutorKind: ExecutorKindNodeRuntime,
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"},
			{OS: "darwin", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "openclaw", Version: 1},
		PackageID: "openclaw", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindOpenClaw,
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "hermes-agent", Version: 1},
		PackageID: "hermes-agent", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindHermes,
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
		},
	},
}

func Contracts() []Contract {
	result := make([]Contract, len(registered))
	for i, contract := range registered {
		result[i] = contract
		result[i].Platforms = append([]appcatalog.Platform{}, contract.Platforms...)
	}
	return result
}

func Lookup(adapter appcatalog.Adapter) (Contract, bool) {
	for _, contract := range registered {
		if contract.Adapter == adapter {
			contract.Platforms = append([]appcatalog.Platform{}, contract.Platforms...)
			return contract, true
		}
	}
	return Contract{}, false
}

func SupportsManifest(manifest appcatalog.Manifest) bool {
	contract, ok := Lookup(manifest.Adapter)
	if !ok || manifest.ID != contract.PackageID || manifest.Kind != contract.PackageKind {
		return false
	}
	supported := make(map[appcatalog.Platform]struct{}, len(contract.Platforms))
	for _, platform := range contract.Platforms {
		supported[platform] = struct{}{}
	}
	for _, platform := range manifest.Platforms {
		if _, ok := supported[platform]; !ok {
			return false
		}
	}
	return true
}

func HasExecutorForPlatform(platform appcatalog.Platform) bool {
	for _, contract := range registered {
		for _, supported := range contract.Platforms {
			if supported == platform {
				return true
			}
		}
	}
	return false
}

func ExecutorKinds() []string {
	seen := make(map[string]struct{}, len(registered))
	for _, contract := range registered {
		seen[contract.ExecutorKind] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for kind := range seen {
		result = append(result, kind)
	}
	sort.Strings(result)
	return result
}
