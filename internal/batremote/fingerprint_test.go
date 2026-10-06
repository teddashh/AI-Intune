package batremote

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeFingerprint(t *testing.T) {
	want := strings.TrimSuffix(strings.Repeat("AB:01:", 16), ":")
	for name, input := range map[string]string{
		"canonical":        want,
		"lowercase":        strings.ToLower(want),
		"no separators":    strings.ReplaceAll(want, ":", ""),
		"spaces":           strings.ReplaceAll(want, ":", " "),
		"newlines":         strings.ReplaceAll(want, ":", "\n"),
		"other separators": strings.ReplaceAll(want, ":", "-_/☃"),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeFingerprint(input)
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
	for name, input := range map[string]string{
		"31 bytes": strings.Repeat("AB", 31),
		"33 bytes": strings.Repeat("AB", 33),
		"empty":    "",
		"odd hex":  strings.Repeat("AB", 31) + "A",
		"no hex":   "xyz !",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeFingerprint(input)
			if got != "" || !errors.Is(err, ErrFingerprintFormat) {
				t.Fatalf("got %q, %v; want fingerprint format error", got, err)
			}
		})
	}
}
