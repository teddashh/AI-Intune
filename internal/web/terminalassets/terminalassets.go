// Package terminalassets is the terminal document's renderer and page script.
//
// The two libraries are vendored UMD builds. Both assign their exports onto
// globalThis, so the page reads globalThis.Terminal and globalThis.FitAddon
// with no module loader. Neither build calls new Worker, fetch, eval, or
// new Function. The importScripts text in xterm is a typeof feature test and
// is never invoked, which is why the terminal policy has no worker-src and no
// network source besides the one WebSocket authority.
//
// Provenance:
//
//	@xterm/xterm 6.0.0
//	https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz
//	tarball sha256 908e66e04af6c8dc6b00dd3b54de088e2e81e5ed866284fd6c2fb3c2d1c7a3f6
//	npm integrity sha512-TQwDdQGtwwDt+2cgKDLn0IRaSxYu1tSUjgKarSDkUM0ZNiSRXFpjxEsvc/Zgc5kq5omJ+V0a8/kIM2WD3sMOYg==
//
//	@xterm/addon-fit 0.11.0
//	https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz
//	tarball sha256 26003b4517a132b64e4ff228fd88a5fda3fff5e606c76093f6dcff772e9ecec0
//	npm integrity sha512-jYcgT6xtVYhnhgxh3QgYDnnNMYTcf8ElbxxFzX0IZo+vabQqSPAjC3c1wJrKB5E19VwQei89QCiZZP86DCPF7g==
//
// PageJS is static. Interpolating a request into it would change its bytes
// and therefore its Content-Security-Policy hash. The hashes below are taken
// once from these embedded bytes; the document emits those bytes unchanged.
package terminalassets

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
)

//go:embed xterm-6.0.0.js xterm-addon-fit-0.11.0.js xterm-6.0.0.css terminal.js
var files embed.FS

var (
	XTermJS  = mustRead("xterm-6.0.0.js")
	FitJS    = mustRead("xterm-addon-fit-0.11.0.js")
	XTermCSS = mustRead("xterm-6.0.0.css")
	PageJS   = mustRead("terminal.js")

	// CSP source tokens, computed once from the embedded bytes.
	XTermJSHash = cspHash(XTermJS)
	FitJSHash   = cspHash(FitJS)
	PageJSHash  = cspHash(PageJS)
)

func mustRead(name string) []byte {
	body, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return body
}

func cspHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}
