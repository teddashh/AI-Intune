package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Render writes the deterministic conf the agent executes. The returned
// digest is sha256 of those exact bytes, including the trailing newline,
// in the form the script prints as config_digest.
func Render(p Profile) (conf []byte, digest string, err error) {
	if err := p.Validate(); err != nil {
		return nil, "", err
	}
	p = p.normalized()
	var b strings.Builder
	b.WriteString("# clawctl disk-clean conf\n")
	b.WriteString("# schema_version=1\n")
	writeKV(&b, "DRY_RUN", bool01(*p.DryRun))
	writeQuoted(&b, "APPLY_CATEGORIES", strings.Join(p.ApplyCategories, " "))
	writeQuoted(&b, "CATEGORIES", strings.Join(p.Categories, " "))
	if p.Scope == ScopeUser {
		writeQuoted(&b, "TMP_DIRS", strings.Join(p.TmpDirs, " "))
	}
	writeKV(&b, "TMP_AGE_DAYS", fmt.Sprintf("%d", p.TmpAgeDays))
	if p.Scope == ScopeUser {
		writeKV(&b, "NPM_CLEAN_MIN_MB", fmt.Sprintf("%d", *p.NpmCleanMinMB))
		writeKV(&b, "PIP_CACHE_MIN_MB", fmt.Sprintf("%d", *p.PipCacheMinMB))
		writeKV(&b, "GO_CACHE_MIN_MB", fmt.Sprintf("%d", *p.GoCacheMinMB))
		writeKV(&b, "THUMB_AGE_DAYS", fmt.Sprintf("%d", *p.ThumbAgeDays))
		writeKV(&b, "TRASH_AGE_DAYS", fmt.Sprintf("%d", *p.TrashAgeDays))
	} else {
		writeKV(&b, "VARTMP_AGE_DAYS", fmt.Sprintf("%d", *p.VarTmpAgeDays))
		writeKV(&b, "JOURNAL_MAX_SIZE", p.JournalMaxSize)
		writeKV(&b, "JOURNAL_MAX_AGE", p.JournalMaxAge)
		writeKV(&b, "DOCKER_UNTIL_HOURS", fmt.Sprintf("%d", *p.DockerUntilHours))
	}
	writeKV(&b, "ATTENTION_PCT", fmt.Sprintf("%d", p.AttentionPct))
	writeKV(&b, "MOUNT", "/")
	if p.Scope == ScopeUser {
		writeKV(&b, "DU_TIMEOUT_S", fmt.Sprintf("%d", *p.DuTimeoutS))
		writeKV(&b, "DU_DEPTH", fmt.Sprintf("%d", *p.DuDepth))
		writeQuoted(&b, "REPORT_PATHS", strings.Join(p.ReportPaths, " "))
	}
	writeQuoted(&b, "PROTECT_NAMES", renderedProtect(p.ExtraProtectNames))
	if p.Scope == ScopeUser {
		writeKV(&b, "ROOT_SUMMARY", RootSummaryPath)
		writeKV(&b, "ROOT_MAX_AGE_H", fmt.Sprintf("%d", RootMaxAgeHours))
		for _, rule := range p.TmpGlobRules {
			fmt.Fprintf(&b, "TMP_GLOB_RULE=%s|%d|%d|%s\n", rule.Glob, rule.MinAgeDays, rule.KeepNewest, rule.BusyRegex)
		}
	}
	out := []byte(b.String())
	if len(out) > MaxConfBytes {
		return nil, "", invalidf("rendered conf exceeds %d bytes", MaxConfBytes)
	}
	return out, Digest(out), nil
}

// Digest is the script's config_digest for these exact conf bytes.
func Digest(conf []byte) string {
	sum := sha256.Sum256(conf)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func renderedProtect(extra []string) string {
	parts := append([]string(nil), BuiltinProtect...)
	parts = append(parts, extra...)
	return strings.Join(parts, " ")
}

func writeKV(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "%s=%s\n", key, value)
}

func writeQuoted(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "%s=\"%s\"\n", key, value)
}

func bool01(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
