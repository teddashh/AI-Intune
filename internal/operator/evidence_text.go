package operator

// EvidenceText is one bounded, scrubbed operator-visible string.
type EvidenceText struct {
	Text      string   `json:"text"`
	MaxBytes  int      `json:"max_bytes"`
	Bytes     int      `json:"bytes"` // ledger byte count before scrubbing
	Truncated bool     `json:"truncated"`
	Issues    []string `json:"issues"`
}

// JobEvidenceText is retained as an alias so the shipped job DTO keeps its name.
type JobEvidenceText = EvidenceText

type evidenceTextPolicy struct {
	maxBytes int
	policy   boundedTextPolicy
}

func projectEvidenceText(spec evidenceTextPolicy, value string) EvidenceText {
	originalBytes := len(value)
	var text string
	var truncated bool
	var issues []string
	if spec.policy == boundedTextBlock {
		text, truncated, issues = scrubBoundedBlockText(value, spec.maxBytes)
	} else {
		text, truncated, issues = scrubBoundedText(value, spec.maxBytes)
	}
	return EvidenceText{
		Text: text, MaxBytes: spec.maxBytes, Bytes: originalBytes, Truncated: truncated, Issues: issues,
	}
}
