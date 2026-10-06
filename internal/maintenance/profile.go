package maintenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	SchemaVersion = 1
	ResourceKind  = "maintenance"
	ResourceID    = "disk-clean"
	JobKind       = "maintenance"

	// VerificationRuleID is the executor evidence rule. The Hub reads this
	// rule's stdout as the fleet-disk-clean/v1 summary.
	VerificationRuleID = "disk_clean_summary"

	ScopeUser = "user"
	ScopeRoot = "root"

	SummarySchema = "fleet-disk-clean/v1"
	ScriptVersion = "1.1.0"

	// StaleAfter is how long a machine may go without a disk-clean summary
	// before the Hub alerts. It is not a profile key. The same window is
	// rendered as ROOT_MAX_AGE_H for the embedded root summary.
	RootSummaryPath = "/var/lib/disk-clean/last.json"
	RootMaxAgeHours = 48

	MaxSummaryBytes         = 32 << 10
	MaxProfileBytes         = 64 << 10
	MaxConfBytes            = 16 << 10
	ExecutionTimeoutSeconds = 1800

	MaxExtraProtect = 32
	MaxGlobRules    = 32
	MaxReportPaths  = 16
	MaxTokenLen     = 64
	MaxBusyRegex    = 128

	MinTmpAgeDays   = 1
	MaxTmpAgeDays   = 3650
	MinAttentionPct = 1
	MaxAttentionPct = 100
	MinDuTimeoutS   = 1
	MaxDuTimeoutS   = 3600
	MinDuDepth      = 1
	MaxDuDepth      = 4
	MinCacheMB      = 0
	MaxCacheMB      = 1 << 20
	MinDockerHours  = 1
	MaxDockerHours  = 8760
	MinKeepNewest   = 0
	MaxKeepNewest   = 1000
)

// UserCategories is the script's user-scope allowlist. Docker volume,
// container, and system-prune categories are not on it and cannot be added.
var UserCategories = []string{
	"user_tmp", "tmp_globs", "npm_cache", "pip_cache", "uv_cache", "go_cache", "thumbnails", "trash",
}

// RootCategories is the script's root-scope allowlist. cat_docker prunes
// dangling images and old build cache only.
var RootCategories = []string{
	"tmpfiles", "journal", "pkg_cache", "docker", "snap",
}

// AllowedTmpDirs is the only pair of directories a profile may name.
var AllowedTmpDirs = []string{"/tmp", "/var/tmp"}

var ErrInvalid = errors.New("maintenance: invalid profile")

// Profile is the closed maintenance document. Unknown JSON fields are refused.
// Protected names are not a field: the Hub always renders BuiltinProtect, and
// ExtraProtectNames can only add to that list.
type Profile struct {
	SchemaVersion int    `json:"schema_version"`
	Scope         string `json:"scope"`
	DryRun        *bool  `json:"dry_run"`

	Categories      []string `json:"categories"`
	ApplyCategories []string `json:"apply_categories,omitempty"`

	TmpDirs    []string `json:"tmp_dirs,omitempty"`
	TmpAgeDays int      `json:"tmp_age_days"`

	NpmCleanMinMB *int `json:"npm_clean_min_mb,omitempty"`
	PipCacheMinMB *int `json:"pip_cache_min_mb,omitempty"`
	GoCacheMinMB  *int `json:"go_cache_min_mb,omitempty"`
	ThumbAgeDays  *int `json:"thumb_age_days,omitempty"`
	TrashAgeDays  *int `json:"trash_age_days,omitempty"`
	DuTimeoutS    *int `json:"du_timeout_s,omitempty"`
	DuDepth       *int `json:"du_depth,omitempty"`

	VarTmpAgeDays    *int   `json:"vartmp_age_days,omitempty"`
	JournalMaxSize   string `json:"journal_max_size,omitempty"`
	JournalMaxAge    string `json:"journal_max_age,omitempty"`
	DockerUntilHours *int   `json:"docker_until_hours,omitempty"`

	AttentionPct int    `json:"attention_pct"`
	Mount        string `json:"mount"`

	ExtraProtectNames []string      `json:"extra_protect_names,omitempty"`
	TmpGlobRules      []TmpGlobRule `json:"tmp_glob_rules,omitempty"`
	ReportPaths       []string      `json:"report_paths,omitempty"`
}

// TmpGlobRule is one user-scope rule for a directory directly inside a tmp dir.
type TmpGlobRule struct {
	Glob       string `json:"glob"`
	MinAgeDays int    `json:"min_age_days"`
	KeepNewest int    `json:"keep_newest"`
	BusyRegex  string `json:"busy_regex,omitempty"`
}

var (
	protectToken = regexp.MustCompile(`^[A-Za-z0-9.*_?-]+$`)
	tmpGlobPath  = regexp.MustCompile(`^/(?:tmp|var/tmp)/[A-Za-z0-9.*_?-]+$`)
	reportPath   = regexp.MustCompile(`^\$HOME(?:/[A-Za-z0-9._*-]+)+$`)
	journalSize  = regexp.MustCompile(`^[1-9][0-9]{0,5}[KMGT]$`)
	journalAge   = regexp.MustCompile(`^[1-9][0-9]{0,3}d$`)
)

// ParseProfile decodes one closed document and returns the normalized profile.
func ParseProfile(raw []byte) (Profile, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Profile{}, invalidf("profile JSON is empty")
	}
	if len(raw) > MaxProfileBytes {
		return Profile{}, invalidf("profile JSON exceeds %d bytes", MaxProfileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return Profile{}, invalidf("profile JSON: %s", err.Error())
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Profile{}, invalidf("profile JSON has trailing data")
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p.normalized(), nil
}

// Canonical is the stable JSON of a valid profile, used for preview digests.
func (p Profile) Canonical() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p.normalized())
	if err != nil {
		return nil, invalidf("profile JSON: %s", err.Error())
	}
	return raw, nil
}

// Validate reports the first reason this document cannot be published.
// The message names the key and the bound.
func (p Profile) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return invalidf("schema_version must be %d", SchemaVersion)
	}
	if p.Scope != ScopeUser && p.Scope != ScopeRoot {
		return invalidf("scope must be %q or %q", ScopeUser, ScopeRoot)
	}
	if p.DryRun == nil {
		return invalidf("dry_run is required")
	}
	if p.Mount != "/" {
		return invalidf("mount must be \"/\"")
	}
	if p.TmpAgeDays < MinTmpAgeDays || p.TmpAgeDays > MaxTmpAgeDays {
		return invalidf("tmp_age_days must be between %d and %d", MinTmpAgeDays, MaxTmpAgeDays)
	}
	if p.AttentionPct < MinAttentionPct || p.AttentionPct > MaxAttentionPct {
		return invalidf("attention_pct must be between %d and %d", MinAttentionPct, MaxAttentionPct)
	}
	allow := UserCategories
	if p.Scope == ScopeRoot {
		allow = RootCategories
	}
	if err := validateCategories("categories", p.Categories, allow, true); err != nil {
		return err
	}
	if err := validateCategories("apply_categories", p.ApplyCategories, p.Categories, false); err != nil {
		return err
	}
	if len(p.ApplyCategories) > 0 && *p.DryRun == false {
		return invalidf("apply_categories is only valid when dry_run is true")
	}
	if err := validateProtectExtras(p.ExtraProtectNames); err != nil {
		return err
	}
	if p.Scope == ScopeUser {
		return p.validateUser()
	}
	return p.validateRoot()
}

func (p Profile) validateUser() error {
	if err := absent(p.VarTmpAgeDays != nil, "vartmp_age_days", "root"); err != nil {
		return err
	}
	if err := absent(p.JournalMaxSize != "", "journal_max_size", "root"); err != nil {
		return err
	}
	if err := absent(p.JournalMaxAge != "", "journal_max_age", "root"); err != nil {
		return err
	}
	if err := absent(p.DockerUntilHours != nil, "docker_until_hours", "root"); err != nil {
		return err
	}
	if err := validateTmpDirs(p.TmpDirs); err != nil {
		return err
	}
	if err := requireInt("npm_clean_min_mb", p.NpmCleanMinMB, MinCacheMB, MaxCacheMB); err != nil {
		return err
	}
	if err := requireInt("pip_cache_min_mb", p.PipCacheMinMB, MinCacheMB, MaxCacheMB); err != nil {
		return err
	}
	if err := requireInt("go_cache_min_mb", p.GoCacheMinMB, MinCacheMB, MaxCacheMB); err != nil {
		return err
	}
	if err := requireInt("thumb_age_days", p.ThumbAgeDays, MinTmpAgeDays, MaxTmpAgeDays); err != nil {
		return err
	}
	if err := requireInt("trash_age_days", p.TrashAgeDays, MinTmpAgeDays, MaxTmpAgeDays); err != nil {
		return err
	}
	if err := requireInt("du_timeout_s", p.DuTimeoutS, MinDuTimeoutS, MaxDuTimeoutS); err != nil {
		return err
	}
	if err := requireInt("du_depth", p.DuDepth, MinDuDepth, MaxDuDepth); err != nil {
		return err
	}
	if len(p.ReportPaths) > MaxReportPaths {
		return invalidf("report_paths accepts at most %d entries", MaxReportPaths)
	}
	seenPath := map[string]struct{}{}
	for i, item := range p.ReportPaths {
		if !reportPath.MatchString(item) || strings.Contains(item, "..") {
			return invalidf("report_paths[%d] must be a $HOME-relative report path with no ..", i)
		}
		if _, ok := seenPath[item]; ok {
			return invalidf("report_paths[%d] duplicates %q", i, item)
		}
		seenPath[item] = struct{}{}
	}
	if len(p.TmpGlobRules) > MaxGlobRules {
		return invalidf("tmp_glob_rules accepts at most %d rules", MaxGlobRules)
	}
	dirs := map[string]struct{}{}
	for _, dir := range p.TmpDirs {
		dirs[dir] = struct{}{}
	}
	seenGlob := map[string]struct{}{}
	for i, rule := range p.TmpGlobRules {
		if !tmpGlobPath.MatchString(rule.Glob) || strings.Contains(rule.Glob, "..") {
			return invalidf("tmp_glob_rules[%d].glob must be one path segment directly under /tmp or /var/tmp", i)
		}
		parent, base := splitParent(rule.Glob)
		if _, ok := dirs[parent]; !ok {
			return invalidf("tmp_glob_rules[%d].glob %q is not directly inside tmp_dirs", i, rule.Glob)
		}
		if strings.HasPrefix(base, "-") {
			return invalidf("tmp_glob_rules[%d].glob %q must not start with '-'", i, rule.Glob)
		}
		if GlobTargetsProtected(rule.Glob) {
			return invalidf("tmp_glob_rules[%d].glob %q targets protected data", i, rule.Glob)
		}
		if _, ok := seenGlob[rule.Glob]; ok {
			return invalidf("tmp_glob_rules[%d].glob duplicates %q", i, rule.Glob)
		}
		seenGlob[rule.Glob] = struct{}{}
		if rule.MinAgeDays < MinTmpAgeDays || rule.MinAgeDays > MaxTmpAgeDays {
			return invalidf("tmp_glob_rules[%d].min_age_days must be between %d and %d", i, MinTmpAgeDays, MaxTmpAgeDays)
		}
		if rule.KeepNewest < MinKeepNewest || rule.KeepNewest > MaxKeepNewest {
			return invalidf("tmp_glob_rules[%d].keep_newest must be between %d and %d", i, MinKeepNewest, MaxKeepNewest)
		}
		if err := validateBusy(i, rule.BusyRegex); err != nil {
			return err
		}
	}
	return nil
}

func (p Profile) validateRoot() error {
	if len(p.TmpDirs) > 0 {
		return invalidf("tmp_dirs is user scope only")
	}
	if len(p.TmpGlobRules) > 0 {
		return invalidf("tmp_glob_rules is user scope only")
	}
	if len(p.ReportPaths) > 0 {
		return invalidf("report_paths is user scope only")
	}
	if err := absent(p.NpmCleanMinMB != nil, "npm_clean_min_mb", "user"); err != nil {
		return err
	}
	if err := absent(p.PipCacheMinMB != nil, "pip_cache_min_mb", "user"); err != nil {
		return err
	}
	if err := absent(p.GoCacheMinMB != nil, "go_cache_min_mb", "user"); err != nil {
		return err
	}
	if err := absent(p.ThumbAgeDays != nil, "thumb_age_days", "user"); err != nil {
		return err
	}
	if err := absent(p.TrashAgeDays != nil, "trash_age_days", "user"); err != nil {
		return err
	}
	if err := absent(p.DuTimeoutS != nil, "du_timeout_s", "user"); err != nil {
		return err
	}
	if err := absent(p.DuDepth != nil, "du_depth", "user"); err != nil {
		return err
	}
	if err := requireInt("vartmp_age_days", p.VarTmpAgeDays, MinTmpAgeDays, MaxTmpAgeDays); err != nil {
		return err
	}
	if !journalSize.MatchString(p.JournalMaxSize) {
		return invalidf("journal_max_size must look like 500M (1-999999 followed by K, M, G, or T)")
	}
	if !journalAge.MatchString(p.JournalMaxAge) {
		return invalidf("journal_max_age must look like 30d")
	}
	if err := requireInt("docker_until_hours", p.DockerUntilHours, MinDockerHours, MaxDockerHours); err != nil {
		return err
	}
	return nil
}

func (p Profile) normalized() Profile {
	out := p
	out.Categories = append([]string(nil), p.Categories...)
	if len(p.ApplyCategories) == 0 {
		out.ApplyCategories = nil
	} else {
		out.ApplyCategories = append([]string(nil), p.ApplyCategories...)
	}
	out.TmpDirs = normalizeTmpDirs(p.TmpDirs)
	out.ExtraProtectNames = normalizeExtras(p.ExtraProtectNames)
	if len(p.TmpGlobRules) == 0 {
		out.TmpGlobRules = nil
	} else {
		out.TmpGlobRules = append([]TmpGlobRule(nil), p.TmpGlobRules...)
	}
	if len(p.ReportPaths) == 0 {
		out.ReportPaths = nil
	} else {
		out.ReportPaths = append([]string(nil), p.ReportPaths...)
	}
	dry := false
	if p.DryRun != nil {
		dry = *p.DryRun
	}
	out.DryRun = &dry
	return out
}

func normalizeTmpDirs(in []string) []string {
	has := map[string]bool{}
	for _, dir := range in {
		has[dir] = true
	}
	var out []string
	for _, dir := range AllowedTmpDirs {
		if has[dir] {
			out = append(out, dir)
		}
	}
	return out
}

func normalizeExtras(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	for _, pat := range BuiltinProtect {
		seen[strings.ToLower(pat)] = struct{}{}
	}
	var out []string
	for _, tok := range in {
		low := strings.ToLower(tok)
		if _, ok := seen[low]; ok {
			continue
		}
		seen[low] = struct{}{}
		out = append(out, low)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validateTmpDirs(dirs []string) error {
	if len(dirs) == 0 {
		return invalidf("tmp_dirs is required for scope user and accepts only /tmp and /var/tmp")
	}
	if len(dirs) > len(AllowedTmpDirs) {
		return invalidf("tmp_dirs accepts only /tmp and /var/tmp")
	}
	seen := map[string]struct{}{}
	allowed := map[string]struct{}{"/tmp": {}, "/var/tmp": {}}
	for i, dir := range dirs {
		if _, ok := allowed[dir]; !ok {
			return invalidf("tmp_dirs[%d] %q is not allowed; tmp_dirs accepts only /tmp and /var/tmp", i, dir)
		}
		if _, ok := seen[dir]; ok {
			return invalidf("tmp_dirs[%d] duplicates %q", i, dir)
		}
		seen[dir] = struct{}{}
	}
	return nil
}

func validateCategories(field string, got, allow []string, required bool) error {
	if len(got) == 0 {
		if required {
			return invalidf("%s must name at least one allowlisted category", field)
		}
		return nil
	}
	if len(got) > len(allow) {
		return invalidf("%s has %d entries; the allowlist has %d", field, len(got), len(allow))
	}
	ok := map[string]struct{}{}
	for _, item := range allow {
		ok[item] = struct{}{}
	}
	seen := map[string]struct{}{}
	for i, item := range got {
		if _, allowed := ok[item]; !allowed {
			return invalidf("%s[%d] %q is not an allowlisted category (%s)", field, i, item, strings.Join(allow, ", "))
		}
		if _, dup := seen[item]; dup {
			return invalidf("%s[%d] duplicates %q", field, i, item)
		}
		seen[item] = struct{}{}
	}
	return nil
}

func validateProtectExtras(names []string) error {
	if len(names) > MaxExtraProtect {
		return invalidf("extra_protect_names accepts at most %d entries", MaxExtraProtect)
	}
	seen := map[string]struct{}{}
	for i, tok := range names {
		if len(tok) == 0 || len(tok) > MaxTokenLen || strings.HasPrefix(tok, "-") || !protectToken.MatchString(tok) {
			return invalidf("extra_protect_names[%d] %q must be 1-%d characters from [A-Za-z0-9.*_?-] and must not start with '-'", i, tok, MaxTokenLen)
		}
		low := strings.ToLower(tok)
		if _, dup := seen[low]; dup {
			return invalidf("extra_protect_names[%d] duplicates %q", i, tok)
		}
		seen[low] = struct{}{}
	}
	return nil
}

func validateBusy(i int, expr string) error {
	if expr == "" {
		return nil
	}
	if len(expr) > MaxBusyRegex || strings.ContainsAny(expr, "|\"'\n\r# ") {
		return invalidf("tmp_glob_rules[%d].busy_regex must be at most %d characters and must not contain spaces, quotes, '#', or '|'", i, MaxBusyRegex)
	}
	if _, err := regexp.Compile(expr); err != nil {
		return invalidf("tmp_glob_rules[%d].busy_regex is not a regular expression", i)
	}
	return nil
}

func requireInt(name string, v *int, min, max int) error {
	if v == nil {
		return invalidf("%s is required", name)
	}
	if *v < min || *v > max {
		return invalidf("%s must be between %d and %d", name, min, max)
	}
	return nil
}

func absent(present bool, name, scope string) error {
	if present {
		return invalidf("%s is %s scope only", name, scope)
	}
	return nil
}

func splitParent(glob string) (string, string) {
	i := strings.LastIndex(glob, "/")
	if i < 0 {
		return "", glob
	}
	return glob[:i], glob[i+1:]
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// ExpectedApplyMode is the summary mode an irreversible (non --dry-run) run of
// this validated profile must report: "apply" when dry_run is false, "mixed"
// when dry_run is true with staged apply_categories, otherwise "dry-run".
func (p Profile) ExpectedApplyMode() string {
	if p.DryRun != nil && !*p.DryRun {
		return "apply"
	}
	if len(p.ApplyCategories) > 0 {
		return "mixed"
	}
	return "dry-run"
}
