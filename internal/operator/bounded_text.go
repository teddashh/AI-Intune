package operator

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	boundedTextIssueInvalidUTF8             = "invalid_utf8"
	boundedTextIssueControlOrFormatReplaced = "control_or_format_replaced"
	boundedTextIssueTruncated               = "truncated"
)

type boundedTextPolicy uint8

const (
	boundedTextStrict boundedTextPolicy = iota
	boundedTextBlock
)

// scrubBoundedText applies the shared operator disclosure text policy. The
// returned issues are always non-nil, sorted, and unique.
func scrubBoundedText(value string, maxBytes int) (string, bool, []string) {
	return scrubBoundedTextWithPolicy(value, maxBytes, boundedTextStrict)
}

// scrubBoundedBlockText applies the disclosure policy for preformatted text.
// Line feeds and tabs remain readable; carriage returns and all other control
// and format characters are replaced.
func scrubBoundedBlockText(value string, maxBytes int) (string, bool, []string) {
	return scrubBoundedTextWithPolicy(value, maxBytes, boundedTextBlock)
}

func scrubBoundedTextWithPolicy(value string, maxBytes int, policy boundedTextPolicy) (string, bool, []string) {
	issues := make([]string, 0, 3)
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
		issues = append(issues, boundedTextIssueInvalidUTF8)
	}
	var out strings.Builder
	replacedControl := false
	for _, char := range value {
		allowedBlockWhitespace := policy == boundedTextBlock && (char == '\n' || char == '\t')
		if (unicode.IsControl(char) && !allowedBlockWhitespace) || unicode.Is(unicode.Cf, char) {
			out.WriteRune('�')
			replacedControl = true
			continue
		}
		out.WriteRune(char)
	}
	value = out.String()
	if replacedControl {
		issues = append(issues, boundedTextIssueControlOrFormatReplaced)
	}
	truncated := len(value) > maxBytes
	if truncated {
		value = truncateBoundedUTF8(value, maxBytes)
		issues = append(issues, boundedTextIssueTruncated)
	}
	sort.Strings(issues)
	return value, truncated, issues
}

func truncateBoundedUTF8(value string, maxBytes int) string {
	const marker = "…"
	if len(value) <= maxBytes {
		return value
	}
	budget := maxBytes - len(marker)
	if budget < 0 {
		budget = 0
	}
	cut := 0
	for index := range value {
		if index > budget {
			break
		}
		cut = index
	}
	if cut == 0 && budget >= len(value) {
		cut = len(value)
	}
	return value[:cut] + marker
}
