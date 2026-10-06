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
	ExecutorKindClaudeCode  = "claude-code"
	ExecutorKindCodex       = "codex"
	ExecutorKindGrok        = "grok"
	ExecutorKindBATServer   = "bat-server"
	ExecutorKindAntigravity = "antigravity"
)

type Contract struct {
	Adapter                  appcatalog.Adapter
	PackageID                string
	PackageKind              appcatalog.PackageKind
	ExecutorKind             string
	RequiresMeasuredEvidence bool
	Platforms                []appcatalog.Platform
	// ProbeTool is the command name a probe observation uses for this package.
	// Empty means observations of this package are not joined by tool name.
	ProbeTool string
}

var registered = []Contract{
	{
		Adapter:   appcatalog.Adapter{Name: "node-runtime", Version: 1},
		PackageID: "node-runtime", PackageKind: appcatalog.KindRuntime,
		ExecutorKind: ExecutorKindNodeRuntime, RequiresMeasuredEvidence: true,
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"},
			{OS: "darwin", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"},
			{OS: "windows", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "openclaw", Version: 1},
		PackageID: "openclaw", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindOpenClaw, RequiresMeasuredEvidence: false,
		ProbeTool: "openclaw",
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "hermes-agent", Version: 1},
		PackageID: "hermes-agent", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindHermes, RequiresMeasuredEvidence: false,
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "claude-code", Version: 1},
		PackageID: "claude-code", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindClaudeCode, RequiresMeasuredEvidence: true,
		ProbeTool: "claude",
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"},
			{OS: "darwin", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"},
			{OS: "windows", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "codex", Version: 1},
		PackageID: "codex", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindCodex, RequiresMeasuredEvidence: true,
		ProbeTool: "codex",
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"},
			{OS: "darwin", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"},
			{OS: "windows", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "grok", Version: 1},
		PackageID: "grok", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindGrok, RequiresMeasuredEvidence: true,
		ProbeTool: "grok",
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"},
			{OS: "darwin", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"},
			{OS: "windows", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "bat-server", Version: 1},
		PackageID: "bat-server", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindBATServer, RequiresMeasuredEvidence: true,
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
		},
	},
	{
		Adapter:   appcatalog.Adapter{Name: "antigravity", Version: 1},
		PackageID: "antigravity", PackageKind: appcatalog.KindApp,
		ExecutorKind: ExecutorKindAntigravity, RequiresMeasuredEvidence: true,
		ProbeTool: "agy",
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"},
			{OS: "darwin", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"},
			{OS: "windows", Arch: "arm64"},
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

func ExecutorAvailableOnGOOS(kind, goos string) bool {
	if kind == "" || goos == "" {
		return false
	}
	for _, contract := range registered {
		if contract.ExecutorKind != kind {
			continue
		}
		for _, platform := range contract.Platforms {
			if platform.OS == goos {
				return true
			}
		}
	}
	return false
}

func ExecutorKindRegistered(kind string) bool {
	if kind == "" {
		return false
	}
	for _, contract := range registered {
		if contract.ExecutorKind == kind {
			return true
		}
	}
	return false
}

// PackageForProbeTool returns the package ID for a probe tool name.
// Names the registry does not declare are left unmapped.
func PackageForProbeTool(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	var found string
	for _, contract := range registered {
		if contract.ProbeTool != name {
			continue
		}
		if found != "" {
			return "", false
		}
		found = contract.PackageID
	}
	if found == "" {
		return "", false
	}
	return found, true
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
