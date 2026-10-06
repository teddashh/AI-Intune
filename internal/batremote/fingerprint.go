package batremote

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

var (
	ErrFingerprintFormat   = errors.New("batremote: invalid fingerprint format")
	ErrFingerprintMismatch = errors.New("batremote: fingerprint mismatch")
)

// NormalizeFingerprint returns uppercase colon-separated SHA-256 hex.
// Non-hex characters are ignored; exactly 32 bytes must remain.
func NormalizeFingerprint(s string) (string, error) {
	var digits strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			digits.WriteRune(c)
		}
	}
	if digits.Len() != sha256.Size*2 {
		return "", ErrFingerprintFormat
	}
	h := strings.ToUpper(digits.String())
	var result strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i != 0 {
			result.WriteByte(':')
		}
		result.WriteString(h[i : i+2])
	}
	return result.String(), nil
}

func certificateFingerprint(der []byte) string {
	digest := sha256.Sum256(der)
	fingerprint, _ := NormalizeFingerprint(hex.EncodeToString(digest[:]))
	return fingerprint
}
