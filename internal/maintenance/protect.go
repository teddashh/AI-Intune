package maintenance

import (
	"path"
	"strings"
)

// BuiltinProtect is the closed set of basename globs disk-clean must never
// delete. The Hub renders every entry into the conf. A profile can add names.
// It cannot remove these. The same list is hard-coded in the script as
// BUILTIN_PROTECT; TestBuiltinProtectMatchesScript fails if they drift.
//
// The list starts from the fleet-disk-clean PROTECT_NAMES default and the
// README "Never touched" set: backups, snapshots, databases (postgres, odoo),
// agent state, sockets, pids, locks, systemd-private, tmux, ssh, X11/ICE.
var BuiltinProtect = []string{
	"systemd-private-*",
	".X11-unix",
	".ICE-unix",
	".XIM-unix",
	".font-unix",
	".Test-unix",
	"tmux-*",
	"ssh-*",
	"snap-private-tmp",
	"*openclaw*",
	"*clawctl*",
	"*claude*",
	"*codex*",
	"*grok*",
	"*agy*",
	"*cursor*",
	"*vscode*",
	"*backup*",
	"*snapshot*",
	"*.sock",
	"*.pid",
	"*.lock",
	".X*-lock",
	".s.PGSQL*",
	"*odoo*",
	"*postgres*",
	"pulse-*",
	"dbus-*",
	"gpg-*",
	"krb5cc*",
	"hsperfdata_*",
}

// BuiltinProtectLine is the space-separated form rendered into PROTECT_NAMES
// and stored in the script's BUILTIN_PROTECT assignment.
func BuiltinProtectLine() string {
	return strings.Join(BuiltinProtect, " ")
}

// GlobTargetsProtected reports whether a TMP_GLOB_RULE glob would match
// protected data. glob is a full path; only the basename is a pattern.
// Matching is case-insensitive. A rule is refused when its basename glob
// matches a protected pattern, when a protected pattern matches the basename,
// or when the basename glob matches a concrete sample of a protected pattern
// (stars removed, each star replaced by x, each star replaced by xx).
func GlobTargetsProtected(glob string) bool {
	base := strings.ToLower(path.Base(glob))
	if base == "" || base == "." || base == ".." || base == "/" {
		return true
	}
	for _, pat := range BuiltinProtect {
		p := strings.ToLower(pat)
		if globMatch(base, p) || globMatch(p, base) {
			return true
		}
		for _, sample := range protectSamples(p) {
			if globMatch(base, sample) {
				return true
			}
		}
	}
	return false
}

func protectSamples(pat string) []string {
	var out []string
	for _, sample := range []string{
		strings.ReplaceAll(pat, "*", ""),
		strings.ReplaceAll(pat, "*", "x"),
		strings.ReplaceAll(pat, "*", "xx"),
	} {
		if sample != "" {
			out = append(out, sample)
		}
	}
	return out
}

func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}
