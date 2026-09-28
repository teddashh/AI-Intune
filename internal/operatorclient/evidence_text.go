package operatorclient

import (
	"fmt"
	"sort"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type evidenceTextPolicy uint8

const (
	evidenceTextStrict evidenceTextPolicy = iota
	evidenceTextBlock
)

type evidenceTextFieldPolicy struct {
	maxBytes int
	policy   evidenceTextPolicy
}

// validateEvidenceText is the single coherence check shared by every evidence
// client. Field maps select the declared byte bound and whitespace policy.
func validateEvidenceText(value operator.EvidenceText, disclosureMaxBytes int, name string, field evidenceTextFieldPolicy) error {
	if value.MaxBytes != field.maxBytes || value.MaxBytes > disclosureMaxBytes ||
		len(value.Text) > value.MaxBytes || value.Bytes < 0 || value.Issues == nil {
		return fmt.Errorf("operator client: %s has invalid text bounds", name)
	}
	if !utf8.ValidString(value.Text) {
		return fmt.Errorf("operator client: %s contains invalid UTF-8", name)
	}
	for _, char := range value.Text {
		allowedBlockWhitespace := field.policy == evidenceTextBlock && (char == '\n' || char == '\t')
		if (unicode.IsControl(char) && !allowedBlockWhitespace) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: %s contains prohibited control or format characters", name)
		}
	}
	if !sort.StringsAreSorted(value.Issues) {
		return fmt.Errorf("operator client: %s issues are not sorted", name)
	}
	hasTruncated := false
	for i, issue := range value.Issues {
		if i > 0 && issue == value.Issues[i-1] {
			return fmt.Errorf("operator client: %s issues are duplicated", name)
		}
		switch issue {
		case "invalid_utf8", "control_or_format_replaced":
		case "truncated":
			hasTruncated = true
		default:
			return fmt.Errorf("operator client: %s contains unknown issue %q", name, issue)
		}
	}
	if value.Truncated != hasTruncated || (len(value.Issues) == 0 && value.Bytes != len(value.Text)) ||
		(value.Truncated && value.Bytes == 0) {
		return fmt.Errorf("operator client: %s truncation metadata is inconsistent", name)
	}
	return nil
}
