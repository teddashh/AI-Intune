package terminalassets

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestVendoredTerminalAssetsKeepTheirBytes(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		hash string
		size int
	}{
		{name: "xterm", body: XTermJS, size: 488663, hash: "14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972"},
		{name: "fit", body: FitJS, size: 1521, hash: "ba3ea256ce0620a0992a197d6c9baea64823fc93d8da07a9e366ca9943c18527"},
		{name: "css", body: XTermCSS, size: 7112, hash: "854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6"},
	} {
		sum := sha256.Sum256(test.body)
		if len(test.body) != test.size || hex.EncodeToString(sum[:]) != test.hash {
			t.Fatalf("%s bytes=%d hash=%s", test.name, len(test.body), hex.EncodeToString(sum[:]))
		}
	}
}

func TestPageScriptIsStaticAndNamesNoSession(t *testing.T) {
	src := string(PageJS)
	if strings.Contains(src, "{{") || strings.Contains(src, "console.") {
		t.Fatal("page script is interpolated or logs")
	}
	for _, needle := range []string{
		`location.protocol === "https:" ? "wss://" : "ws://"`,
		`location.host + location.pathname + "/socket"`,
		`"aiintune.operator-terminal.v1"`,
		"32 * 1024",
		`type: "input"`,
		`type: "resize"`,
		`type === "ready"`,
		`type === "output"`,
		`type === "exit"`,
		`type === "error"`,
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("page script missing %s", needle)
		}
	}
	if strings.Contains(src, `session:`) || strings.Contains(src, `"session"`) {
		t.Fatal("page script names a session")
	}
	if !strings.Contains(src, `while (end > offset && (bytes[end] & 0xC0) === 0x80)`) {
		t.Fatal("input split does not back off a UTF-8 continuation byte")
	}
	start := strings.Index(src, `addEventListener("beforeunload"`)
	if start < 0 {
		t.Fatal("page script does not confirm leaving the terminal")
	}
	handler := src[start:]
	end := strings.Index(handler, "});")
	if end < 0 {
		t.Fatal("beforeunload handler does not close")
	}
	handler = handler[:end]
	guard := strings.Index(handler, "if (!interactive)")
	prevent := strings.Index(handler, "event.preventDefault()")
	value := strings.Index(handler, `event.returnValue = ""`)
	if guard < 0 || prevent < guard || value < prevent {
		t.Fatal("beforeunload confirm is not conditioned on interactive")
	}
}
