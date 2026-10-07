// Package totp implements RFC 6238 SHA1, six-digit, 30-second TOTP.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret generates a 20-byte secret encoded as unpadded base32.
func NewSecret() (string, error) {
	b := make([]byte, 20)
	_, err := rand.Read(b)
	return encoding.EncodeToString(b), err
}

// Code returns the six-digit code for a counter step.
func Code(secret string, step int64) (string, error) {
	key, err := encoding.DecodeString(secret)
	if err != nil {
		return "", err
	}
	if len(key) == 0 || step < 0 {
		return "", fmt.Errorf("invalid TOTP secret or step")
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	h := hmac.New(sha1.New, key)
	h.Write(counter[:])
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 15
	n := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000), nil
}

// Verify accepts the current step and one step on either side, returning the matched step.
// Callers must persist the matched step to reject replay.
func Verify(secret, code string, now time.Time) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	current := now.Unix() / 30
	for _, step := range []int64{current, current - 1, current + 1} {
		want, err := Code(secret, step)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// URI builds an authenticator enrollment URI with escaped label components.
func URI(secret, username, publicHost string) string {
	issuer := "clawctl Hub (" + publicHost + ")"
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + strings.ReplaceAll(url.PathEscape(issuer), ":", "%3A") + ":" + strings.ReplaceAll(url.PathEscape(username), ":", "%3A") + "?" + q.Encode()
}
