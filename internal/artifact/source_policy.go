package artifact

import "fmt"

// artifactSourcePolicyVersions is the only source-kind to policy-version mapping.
// A kind missing from this table is rejected instead of inheriting the npm policy.
var artifactSourcePolicyVersions = map[string]string{
	ArtifactSourceNPM:         FetchPolicyVersion,
	ArtifactSourceNode:        NodeRuntimeFetchPolicyVersion,
	ArtifactSourceHermesImage: HermesImageFetchPolicyVersion,
	ArtifactSourceClaudeCode:  ClaudeCodeFetchPolicyVersion,
	ArtifactSourceCodex:       CodexFetchPolicyVersion,
	ArtifactSourceGrok:        GrokFetchPolicyVersion,
	ArtifactSourceBATServer:   BATServerFetchPolicyVersion,
	ArtifactSourceAntigravity: AntigravityFetchPolicyVersion,
}

// PolicyVersionForSourceKind returns the fetch policy bound to a known source kind.
// An empty or unknown kind returns an error.
func PolicyVersionForSourceKind(sourceKind string) (string, error) {
	policy, ok := artifactSourcePolicyVersions[sourceKind]
	if !ok || policy == "" {
		return "", fmt.Errorf("%w: unknown artifact source kind", ErrInvalidFetchRequest)
	}
	return policy, nil
}

type durableLedgerDisposition int

const (
	durableLedgerUnspecified durableLedgerDisposition = iota
	durableLedgerFits
	durableLedgerExceeds
)

type artifactSourcePreviewBudget struct {
	maxBytes int64
	ledger   durableLedgerDisposition
}

// artifactSourcePreviewBudgets is the only source-kind to preview MaxBytes mapping.
// Each kind sets ledger to durableLedgerFits or durableLedgerExceeds.
// A missing kind or an unspecified ledger is rejected.
var artifactSourcePreviewBudgets = map[string]artifactSourcePreviewBudget{
	ArtifactSourceNPM:         {maxBytes: DefaultArtifactMaxBytes, ledger: durableLedgerFits},
	ArtifactSourceNode:        {maxBytes: DefaultNodeRuntimeBundleMaxBytes, ledger: durableLedgerFits},
	ArtifactSourceClaudeCode:  {maxBytes: DefaultClaudeCodeBundleMaxBytes, ledger: durableLedgerFits},
	ArtifactSourceCodex:       {maxBytes: DefaultCodexBundleMaxBytes, ledger: durableLedgerFits},
	ArtifactSourceGrok:        {maxBytes: DefaultGrokBundleMaxBytes, ledger: durableLedgerFits},
	ArtifactSourceBATServer:   {maxBytes: DefaultBATServerBundleMaxBytes, ledger: durableLedgerFits},
	ArtifactSourceHermesImage: {maxBytes: DefaultHermesImageBundleMaxBytes, ledger: durableLedgerExceeds},
	ArtifactSourceAntigravity: {maxBytes: DefaultAntigravityBundleMaxBytes, ledger: durableLedgerFits},
}

// previewBudgetForSourceKind returns the preview MaxBytes bound to a known source kind
// and whether that budget fits the durable artifact fetch ledger.
func previewBudgetForSourceKind(sourceKind string) (int64, durableLedgerDisposition, error) {
	budget, ok := artifactSourcePreviewBudgets[sourceKind]
	if !ok || budget.maxBytes <= 0 ||
		(budget.ledger != durableLedgerFits && budget.ledger != durableLedgerExceeds) {
		return 0, durableLedgerUnspecified, fmt.Errorf("%w: unknown artifact source kind", ErrInvalidFetchRequest)
	}
	return budget.maxBytes, budget.ledger, nil
}
