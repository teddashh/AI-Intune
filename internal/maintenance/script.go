package maintenance

import _ "embed"

//go:embed script/disk-clean
var embeddedScript string

// ScriptBytes is the disk-clean script embedded in the agent binary.
// The agent writes these bytes to a private path and executes that path.
// It does not run a disk-clean binary found on PATH.
func ScriptBytes() []byte {
	return []byte(embeddedScript)
}
