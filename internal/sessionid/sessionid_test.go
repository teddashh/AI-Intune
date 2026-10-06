package sessionid

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	seen := make(map[string]struct{}, 1024)
	for i := 0; i < 1024; i++ {
		id := New()
		if !Valid(id) {
			t.Fatalf("Valid(New()) = false for %q", id)
		}
		if len(id) != 43 {
			t.Fatalf("len(New()) = %d, want 43", len(id))
		}
		if strings.Trim(id, alphabet) != "" {
			t.Fatalf("New() = %q, want raw URL alphabet only", id)
		}
		raw, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil || len(raw) != 32 {
			t.Fatalf("New() = %q, decode err=%v len=%d", id, err, len(raw))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate New() result %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "empty", id: "", want: false},
		{name: "128 bytes", id: strings.Repeat("s", MaxLength), want: true},
		{name: "129 bytes", id: strings.Repeat("s", MaxLength+1), want: false},
		{name: "invalid UTF-8", id: string([]byte{'s', 0xff}), want: false},
		{name: "NUL", id: "session\x00id", want: false},
		{name: "BEL", id: "session\aid", want: false},
		{name: "CR", id: "session\rid", want: false},
		{name: "LF", id: "session\nid", want: false},
		{name: "DEL", id: "session\x7fid", want: false},
		{name: "line separator", id: "session\u2028id", want: false},
		{name: "paragraph separator", id: "session\u2029id", want: false},
		{name: "valid multi-byte ID", id: "終端-session-α", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Valid(tt.id); got != tt.want {
				t.Fatalf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
