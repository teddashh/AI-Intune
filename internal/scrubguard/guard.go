// Package scrubguard keeps private fleet identifiers out of this repository.
//
// The repository is public. Host names, tailnet addresses, the tailnet name,
// and personal domains of the maintainer's own fleet must never land here,
// not even in tests or comments. The guard cannot list those values in
// plain text (that would publish them), so denylist.txt holds only salted
// SHA-256 digests. TestRepositoryHasNoDenylistedTokens hashes every token in
// every tracked text file and fails on a match.
//
// The digests hide the values from casual reading and search. They do not
// make them secret: a guessed value can be hashed and compared. The
// plain-text list and the generator live outside this repository.
package scrubguard

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Salt is prepended to every candidate before hashing.
const Salt = "ai-intune/scrubguard/v1\x00"

var tokenPattern = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._@-]*`)

// Digest returns the hex digest that denylist.txt stores for value.
func Digest(value string) string {
	sum := sha256.Sum256([]byte(Salt + strings.ToLower(value)))
	return hex.EncodeToString(sum[:])
}

// Candidates returns every string checked for one token: the whole token,
// each piece split on '@', '.', '_', '-', the pieces split on '@', '.', '_'
// only (so "host-a" stays whole), and every dot suffix (so a domain is found
// inside a longer host name or an e-mail address).
func Candidates(token string) []string {
	t := strings.Trim(strings.ToLower(token), ".-_@")
	if t == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	add(t)
	for _, p := range strings.Split(t, "@") {
		add(p)
	}
	for _, p := range strings.FieldsFunc(t, func(r rune) bool { return r == '.' || r == '_' || r == '@' || r == '-' }) {
		add(p)
	}
	for _, p := range strings.FieldsFunc(t, func(r rune) bool { return r == '.' || r == '_' || r == '@' }) {
		add(p)
	}
	labels := strings.Split(t, ".")
	for i := range labels {
		add(strings.Join(labels[i:], "."))
	}
	return out
}

// Hit is one denylisted token found in a file.
type Hit struct {
	Line  int
	Token string
}

// ScanText returns the denylisted tokens in text.
func ScanText(text string, deny map[string]struct{}) []Hit {
	var hits []Hit
	for n, line := range strings.Split(text, "\n") {
		for _, tok := range tokenPattern.FindAllString(line, -1) {
			for _, c := range Candidates(tok) {
				if _, ok := deny[Digest(c)]; ok {
					hits = append(hits, Hit{Line: n + 1, Token: tok})
					break
				}
			}
		}
	}
	return hits
}

// ParseDenylist reads denylist.txt: one lowercase hex digest per line;
// blank lines and lines starting with '#' are ignored.
func ParseDenylist(text string) (map[string]struct{}, []string) {
	deny := map[string]struct{}{}
	var bad []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) != 64 || strings.Trim(line, "0123456789abcdef") != "" {
			bad = append(bad, line)
			continue
		}
		deny[line] = struct{}{}
	}
	return deny, bad
}
