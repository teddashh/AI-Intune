// Package sessionid defines the grammar for a terminal session identifier on
// both the wire and in the ledger. The ledger rule must never be broader than
// the wire rule.
package sessionid

import (
	"crypto/rand"
	"encoding/base64"
	"unicode"
	"unicode/utf8"
)

const MaxLength = 128

// New returns a fresh terminal session identifier.
//
// crypto/rand.Read never returns an error in this toolchain, and it always
// fills the buffer. A failed system source crashes the process, so there is
// no error for New to return or to swallow.
func New() string {
	var raw [32]byte
	rand.Read(raw[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// Valid reports whether id is a valid terminal session identifier.
func Valid(id string) bool {
	if id == "" || len(id) > MaxLength || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}
