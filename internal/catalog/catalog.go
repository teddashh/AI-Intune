// Package catalog defines deployable app/runtime manifests and resolves an
// exact machine profile into a deterministic, dependency-ordered plan.
//
// Catalog input is metadata. Only manifests that name immutable artifact
// bytes and an agent-supported typed adapter can enter a deployment plan.
package catalog

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaVersion       = 1
	MaxDependencies     = 128
	maxCatalogPackages  = 4096
	maxProfilePackages  = 128
	maxListEntries      = 128
	maxArtifactSize     = int64(1 << 40) // 1 TiB; large model-backed apps still fit.
	maxIdentifierBytes  = 128
	maxDisplayTextBytes = 256
)

const (
	// CapabilityAgentRuntime is provided by a machine's primary agent runtime.
	CapabilityAgentRuntime = "agent-runtime"
	// ExclusiveGroupPrimaryAgentRuntime permits exactly one selected provider
	// such as OpenClaw or Hermes in a resolved profile.
	ExclusiveGroupPrimaryAgentRuntime = "primary-agent-runtime"
)

// PackageKind separates reusable runtimes from operator-selected apps.
type PackageKind string

const (
	KindRuntime PackageKind = "runtime"
	KindApp     PackageKind = "app"
)

// Source identifies the catalog and exact upstream revision from which a
// deployable manifest was produced. It is provenance, not executable input.
type Source struct {
	Catalog     string `json:"catalog"`
	UpstreamURL string `json:"upstream_url"`
	Revision    string `json:"revision"`
	License     string `json:"license"`
}

// Artifact identifies immutable bytes already admitted to the Hub catalog.
type Artifact struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Adapter is a typed executor contract implemented by clawctl-agent. Catalog
// manifests cannot contain shell commands or scripts.
type Adapter struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// Platform is one exact operating-system/architecture pair.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// darwinDisplayPrefix 是這棵樹自己的 darwin probe 會產生的 OS 顯示字串
// 開頭（internal/probe/osname_darwin.go 的 parseDarwinSystemVersion）。
// enroll 只把顯示字串帶過來，沒有帶 GOOS，所以今天這是唯一能把 Mac
// 跟 Linux 發行版分開的證據。
const darwinDisplayPrefix = "macOS"

const (
	windowsGOOS            = "windows"
	windowsDisplayExact    = "windows"
	windowsDisplayPrefix   = "windows "
	microsoftWindowsPrefix = "microsoft windows "
)

// PlatformFromProbeIdentity 把 probe 的顯示身分轉成 manifest 與 executable
// job spec 使用的 GOOS/GOARCH 詞彙。Linux 側記錄 /etc/os-release 的
// PRETTY_NAME，darwin 側記錄 SystemVersion.plist；enrollment 只帶顯示字串與
// uname -m，沒有帶 GOOS。已辨識的 macOS 顯示身分必須保留為 darwin，已辨識的
// Windows 顯示身分必須保留為 windows，兩者都不能落入預設的 linux target。
func PlatformFromProbeIdentity(osDisplay, unameArch string) (Platform, error) {
	if osDisplay == "" || osDisplay != strings.TrimSpace(osDisplay) ||
		unameArch == "" || unameArch != strings.TrimSpace(unameArch) {
		return Platform{}, &ResolutionError{Code: CodeInvalidTarget, Detail: "machine platform identity is invalid"}
	}
	arch := ""
	switch unameArch {
	case "amd64", "x86_64":
		arch = "amd64"
	case "arm64", "aarch64":
		arch = "arm64"
	default:
		return Platform{}, &ResolutionError{Code: CodeUnsupportedPlatform, Detail: "machine architecture is unsupported"}
	}
	if strings.HasPrefix(osDisplay, darwinDisplayPrefix) {
		return Platform{OS: "darwin", Arch: arch}, nil
	}
	if windowsProbeIdentity(osDisplay) {
		return Platform{OS: windowsGOOS, Arch: arch}, nil
	}
	return Platform{OS: "linux", Arch: arch}, nil
}

// windowsProbeIdentity 認 canonical Windows 顯示字串：Tailscale 的 "windows"、
// "Windows 11" / "Windows Server …" 這類 prefix，以及 "Microsoft Windows …"。
// 大小寫不敏感；不含空格的 WindowsSomething 不算。
func windowsProbeIdentity(osDisplay string) bool {
	lower := strings.ToLower(osDisplay)
	return lower == windowsDisplayExact ||
		strings.HasPrefix(lower, windowsDisplayPrefix) ||
		lower == "microsoft windows" || strings.HasPrefix(lower, microsoftWindowsPrefix)
}

// PackageRef always selects one exact package version. Version ranges are
// resolved before publication, never independently by each endpoint.
type PackageRef struct {
	PackageID string `json:"package_id"`
	Version   string `json:"version"`
}

// Manifest is the complete deployable record for one package version.
// Provides describes capabilities; ExclusiveGroups enforce select-one slots;
// Conflicts names package IDs that cannot coexist with this package.
type Manifest struct {
	SchemaVersion   int          `json:"schema_version"`
	ID              string       `json:"id"`
	Version         string       `json:"version"`
	Kind            PackageKind  `json:"kind"`
	Title           string       `json:"title"`
	Source          Source       `json:"source"`
	Artifact        Artifact     `json:"artifact"`
	Adapter         Adapter      `json:"adapter"`
	Platforms       []Platform   `json:"platforms"`
	Dependencies    []PackageRef `json:"dependencies"`
	Provides        []string     `json:"provides"`
	Conflicts       []string     `json:"conflicts"`
	ExclusiveGroups []string     `json:"exclusive_groups"`
}

// MachineProfile is the operator-selected desired software set. Enrollment
// binds a machine to one exact profile revision; resolution expands it without
// endpoint-specific package selection.
type MachineProfile struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	Revision      int64        `json:"revision"`
	Packages      []PackageRef `json:"packages"`
}

// PlannedPackage is one dependency-ordered plan entry.
type PlannedPackage struct {
	Manifest   Manifest
	Direct     bool
	RequiredBy []string
}

// Plan contains dependencies before every package that requires them.
type Plan struct {
	ProfileID       string
	ProfileRevision int64
	Target          Platform
	Packages        []PlannedPackage
}

// ResolutionCode is stable machine-readable evidence for a profile that
// cannot be expanded.
type ResolutionCode string

const (
	CodeInvalidProfile         ResolutionCode = "INVALID_PROFILE"
	CodeInvalidTarget          ResolutionCode = "INVALID_TARGET"
	CodeMissingPackage         ResolutionCode = "MISSING_PACKAGE"
	CodeVersionConflict        ResolutionCode = "VERSION_CONFLICT"
	CodeDependencyCycle        ResolutionCode = "DEPENDENCY_CYCLE"
	CodeUnsupportedPlatform    ResolutionCode = "UNSUPPORTED_PLATFORM"
	CodePackageConflict        ResolutionCode = "PACKAGE_CONFLICT"
	CodeExclusiveGroupConflict ResolutionCode = "EXCLUSIVE_GROUP_CONFLICT"
)

// ResolutionError keeps conflict identity out of free-form error parsing.
type ResolutionError struct {
	Code    ResolutionCode
	Package string
	Related string
	Group   string
	Target  Platform
	Detail  string
}

func (e *ResolutionError) Error() string {
	if e == nil {
		return "catalog resolution failed"
	}
	switch e.Code {
	case CodeMissingPackage:
		return fmt.Sprintf("catalog: package %s is missing", e.Package)
	case CodeVersionConflict:
		return fmt.Sprintf("catalog: package versions conflict: %s and %s", e.Package, e.Related)
	case CodeDependencyCycle:
		return fmt.Sprintf("catalog: dependency cycle reaches %s from %s", e.Package, e.Related)
	case CodeUnsupportedPlatform:
		if e.Detail != "" {
			return "catalog: " + e.Detail
		}
		return fmt.Sprintf("catalog: package %s does not support %s/%s", e.Package, e.Target.OS, e.Target.Arch)
	case CodePackageConflict:
		return fmt.Sprintf("catalog: packages conflict: %s and %s", e.Package, e.Related)
	case CodeExclusiveGroupConflict:
		return fmt.Sprintf("catalog: packages %s and %s both occupy %s", e.Package, e.Related, e.Group)
	case CodeInvalidProfile, CodeInvalidTarget:
		return "catalog: " + e.Detail
	default:
		return "catalog: profile resolution failed"
	}
}

// Catalog is an immutable index of validated exact package versions.
type Catalog struct {
	packages map[string]Manifest
}

// New validates a complete catalog and rejects duplicate package identities.
func New(manifests []Manifest) (*Catalog, error) {
	if len(manifests) > maxCatalogPackages {
		return nil, fmt.Errorf("catalog: package count %d exceeds %d", len(manifests), maxCatalogPackages)
	}
	result := &Catalog{packages: make(map[string]Manifest, len(manifests))}
	for i, candidate := range manifests {
		if err := ValidateManifest(candidate); err != nil {
			return nil, fmt.Errorf("catalog: manifest %d: %w", i+1, err)
		}
		candidate = canonicalManifest(candidate)
		identity := packageIdentity(candidate.ID, candidate.Version)
		if _, exists := result.packages[identity]; exists {
			return nil, fmt.Errorf("catalog: duplicate package %s", identity)
		}
		result.packages[identity] = candidate
	}
	return result, nil
}

// ValidateManifest validates one deployable manifest without consulting other
// catalog entries.
func ValidateManifest(m Manifest) error {
	switch {
	case m.SchemaVersion != SchemaVersion:
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	case !validIdentifier(m.ID):
		return errors.New("id is invalid")
	case !validExactText(m.Version, maxIdentifierBytes):
		return errors.New("version is invalid")
	case m.Kind != KindRuntime && m.Kind != KindApp:
		return fmt.Errorf("kind %q is invalid", m.Kind)
	case !validExactText(m.Title, maxDisplayTextBytes):
		return errors.New("title is invalid")
	case errSource(m.Source) != nil:
		return errSource(m.Source)
	case !validSHA256(m.Artifact.SHA256):
		return errors.New("artifact sha256 must be 64 lowercase hexadecimal characters")
	case m.Artifact.Size <= 0 || m.Artifact.Size > maxArtifactSize:
		return fmt.Errorf("artifact size must be between 1 and %d bytes", maxArtifactSize)
	case !validIdentifier(m.Adapter.Name):
		return errors.New("adapter name is invalid")
	case m.Adapter.Version <= 0:
		return errors.New("adapter version must be positive")
	case len(m.Platforms) == 0 || len(m.Platforms) > maxListEntries:
		return fmt.Errorf("platform count must be between 1 and %d", maxListEntries)
	case len(m.Dependencies) > MaxDependencies:
		return fmt.Errorf("dependency count exceeds %d", MaxDependencies)
	case len(m.Provides) == 0 || len(m.Provides) > maxListEntries:
		return fmt.Errorf("provides count must be between 1 and %d", maxListEntries)
	case len(m.Conflicts) > maxListEntries:
		return fmt.Errorf("conflicts count exceeds %d", maxListEntries)
	case len(m.ExclusiveGroups) > maxListEntries:
		return fmt.Errorf("exclusive_groups count exceeds %d", maxListEntries)
	}

	seenPlatforms := make(map[string]struct{}, len(m.Platforms))
	for i, platform := range m.Platforms {
		if !validPlatform(platform) {
			return fmt.Errorf("platforms[%d] is invalid", i)
		}
		key := platform.OS + "/" + platform.Arch
		if _, exists := seenPlatforms[key]; exists {
			return fmt.Errorf("platforms contains duplicate %s", key)
		}
		seenPlatforms[key] = struct{}{}
	}

	seenDependencies := make(map[string]string, len(m.Dependencies))
	for i, dependency := range m.Dependencies {
		if err := validatePackageRef(dependency); err != nil {
			return fmt.Errorf("dependencies[%d]: %w", i, err)
		}
		if dependency.PackageID == m.ID && dependency.Version == m.Version {
			return fmt.Errorf("dependencies[%d] refers to the package itself", i)
		}
		if previous, exists := seenDependencies[dependency.PackageID]; exists {
			if previous == dependency.Version {
				return fmt.Errorf("dependencies contains duplicate %s", packageIdentity(dependency.PackageID, dependency.Version))
			}
			return fmt.Errorf("dependencies selects both %s and %s", packageIdentity(dependency.PackageID, previous), packageIdentity(dependency.PackageID, dependency.Version))
		}
		seenDependencies[dependency.PackageID] = dependency.Version
	}
	if err := validateIdentifierList("provides", m.Provides); err != nil {
		return err
	}
	if err := validateIdentifierList("conflicts", m.Conflicts); err != nil {
		return err
	}
	if err := validateIdentifierList("exclusive_groups", m.ExclusiveGroups); err != nil {
		return err
	}
	providesAgentRuntime := contains(m.Provides, CapabilityAgentRuntime)
	occupiesPrimaryRuntime := contains(m.ExclusiveGroups, ExclusiveGroupPrimaryAgentRuntime)
	if providesAgentRuntime != occupiesPrimaryRuntime {
		return fmt.Errorf("%s and %s must be declared together",
			CapabilityAgentRuntime, ExclusiveGroupPrimaryAgentRuntime)
	}
	for _, conflict := range m.Conflicts {
		if conflict == m.ID {
			return errors.New("conflicts contains the package's own id")
		}
	}
	return nil
}

// ValidateProfile validates the bounded exact selections in a machine profile.
func ValidateProfile(profile MachineProfile) error {
	switch {
	case profile.SchemaVersion != SchemaVersion:
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	case !validIdentifier(profile.ID):
		return errors.New("id is invalid")
	case profile.Revision <= 0:
		return errors.New("revision must be positive")
	case len(profile.Packages) == 0 || len(profile.Packages) > maxProfilePackages:
		return fmt.Errorf("package count must be between 1 and %d", maxProfilePackages)
	}
	seen := make(map[string]struct{}, len(profile.Packages))
	for i, selected := range profile.Packages {
		if err := validatePackageRef(selected); err != nil {
			return fmt.Errorf("packages[%d]: %w", i, err)
		}
		identity := packageIdentity(selected.PackageID, selected.Version)
		if _, exists := seen[identity]; exists {
			return fmt.Errorf("packages contains duplicate %s", identity)
		}
		seen[identity] = struct{}{}
	}
	return nil
}

// Resolve expands one exact profile for one target. Resolution is deterministic
// regardless of manifest or profile input order.
func (c *Catalog) Resolve(profile MachineProfile, target Platform) (Plan, error) {
	if err := ValidateProfile(profile); err != nil {
		return Plan{}, &ResolutionError{Code: CodeInvalidProfile, Detail: err.Error()}
	}
	if !validPlatform(target) {
		return Plan{}, &ResolutionError{Code: CodeInvalidTarget, Detail: "target is invalid", Target: target}
	}
	if c == nil || c.packages == nil {
		return Plan{}, &ResolutionError{Code: CodeMissingPackage, Package: packageIdentity(profile.Packages[0].PackageID, profile.Packages[0].Version)}
	}

	roots := append([]PackageRef(nil), profile.Packages...)
	sortPackageRefs(roots)
	direct := make(map[string]bool, len(roots))
	for _, root := range roots {
		direct[packageIdentity(root.PackageID, root.Version)] = true
	}

	const (
		unvisited uint8 = iota
		visiting
		visited
	)
	states := make(map[string]uint8)
	selectedVersions := make(map[string]string)
	requiredBy := make(map[string]map[string]struct{})
	ordered := make([]Manifest, 0, len(roots))

	var visit func(PackageRef, string) error
	visit = func(ref PackageRef, parent string) error {
		identity := packageIdentity(ref.PackageID, ref.Version)
		if previous, exists := selectedVersions[ref.PackageID]; exists && previous != ref.Version {
			return &ResolutionError{
				Code: CodeVersionConflict, Package: packageIdentity(ref.PackageID, previous), Related: identity,
			}
		}
		selectedVersions[ref.PackageID] = ref.Version
		if parent != "" {
			if requiredBy[identity] == nil {
				requiredBy[identity] = make(map[string]struct{})
			}
			requiredBy[identity][parent] = struct{}{}
		}

		manifest, exists := c.packages[identity]
		if !exists {
			return &ResolutionError{Code: CodeMissingPackage, Package: identity, Related: parent}
		}
		if !supports(manifest, target) {
			return &ResolutionError{Code: CodeUnsupportedPlatform, Package: identity, Related: parent, Target: target}
		}
		switch states[identity] {
		case visiting:
			return &ResolutionError{Code: CodeDependencyCycle, Package: identity, Related: parent}
		case visited:
			return nil
		}

		states[identity] = visiting
		dependencies := append([]PackageRef(nil), manifest.Dependencies...)
		sortPackageRefs(dependencies)
		for _, dependency := range dependencies {
			if err := visit(dependency, identity); err != nil {
				return err
			}
		}
		states[identity] = visited
		ordered = append(ordered, manifest)
		return nil
	}

	for _, root := range roots {
		if err := visit(root, ""); err != nil {
			return Plan{}, err
		}
	}

	groupOwner := make(map[string]string)
	for _, manifest := range ordered {
		identity := packageIdentity(manifest.ID, manifest.Version)
		for _, group := range manifest.ExclusiveGroups {
			if owner, exists := groupOwner[group]; exists && owner != identity {
				return Plan{}, &ResolutionError{
					Code: CodeExclusiveGroupConflict, Package: owner, Related: identity, Group: group,
				}
			}
			groupOwner[group] = identity
		}
	}

	activeByID := make(map[string]string, len(ordered))
	for _, manifest := range ordered {
		activeByID[manifest.ID] = packageIdentity(manifest.ID, manifest.Version)
	}
	for _, manifest := range ordered {
		identity := packageIdentity(manifest.ID, manifest.Version)
		for _, conflictID := range manifest.Conflicts {
			if conflict, exists := activeByID[conflictID]; exists {
				return Plan{}, &ResolutionError{Code: CodePackageConflict, Package: identity, Related: conflict}
			}
		}
	}

	plan := Plan{
		ProfileID: profile.ID, ProfileRevision: profile.Revision, Target: target,
		Packages: make([]PlannedPackage, 0, len(ordered)),
	}
	for _, manifest := range ordered {
		identity := packageIdentity(manifest.ID, manifest.Version)
		parents := make([]string, 0, len(requiredBy[identity]))
		for parent := range requiredBy[identity] {
			parents = append(parents, parent)
		}
		sort.Strings(parents)
		plan.Packages = append(plan.Packages, PlannedPackage{
			Manifest: canonicalManifest(manifest), Direct: direct[identity], RequiredBy: parents,
		})
	}
	return plan, nil
}

func errSource(source Source) error {
	parsed, err := url.Parse(source.UpstreamURL)
	switch {
	case !validSourceCatalog(source.Catalog):
		return errors.New("source catalog is invalid")
	case !validExactText(source.Revision, maxIdentifierBytes):
		return errors.New("source revision is invalid")
	case !validExactText(source.License, maxIdentifierBytes):
		return errors.New("source license is invalid")
	case err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "":
		return errors.New("source upstream_url must be an HTTPS URL without credentials, query, or fragment")
	case !validExactText(source.UpstreamURL, 2048):
		return errors.New("source upstream_url is invalid")
	default:
		return nil
	}
}

func validatePackageRef(ref PackageRef) error {
	if !validIdentifier(ref.PackageID) {
		return errors.New("package_id is invalid")
	}
	if !validExactText(ref.Version, maxIdentifierBytes) {
		return errors.New("version is invalid")
	}
	return nil
}

func validateIdentifierList(name string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if !validIdentifier(value) {
			return fmt.Errorf("%s[%d] is invalid", name, i)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate %s", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func canonicalManifest(manifest Manifest) Manifest {
	manifest.Platforms = append(make([]Platform, 0, len(manifest.Platforms)), manifest.Platforms...)
	manifest.Dependencies = append(make([]PackageRef, 0, len(manifest.Dependencies)), manifest.Dependencies...)
	manifest.Provides = append(make([]string, 0, len(manifest.Provides)), manifest.Provides...)
	manifest.Conflicts = append(make([]string, 0, len(manifest.Conflicts)), manifest.Conflicts...)
	manifest.ExclusiveGroups = append(make([]string, 0, len(manifest.ExclusiveGroups)), manifest.ExclusiveGroups...)
	sort.Slice(manifest.Platforms, func(i, j int) bool {
		if manifest.Platforms[i].OS != manifest.Platforms[j].OS {
			return manifest.Platforms[i].OS < manifest.Platforms[j].OS
		}
		return manifest.Platforms[i].Arch < manifest.Platforms[j].Arch
	})
	sortPackageRefs(manifest.Dependencies)
	sort.Strings(manifest.Provides)
	sort.Strings(manifest.Conflicts)
	sort.Strings(manifest.ExclusiveGroups)
	return manifest
}

func sortPackageRefs(refs []PackageRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].PackageID != refs[j].PackageID {
			return refs[i].PackageID < refs[j].PackageID
		}
		return refs[i].Version < refs[j].Version
	})
}

func supports(manifest Manifest, target Platform) bool {
	for _, candidate := range manifest.Platforms {
		if candidate == target {
			return true
		}
	}
	return false
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func packageIdentity(id, version string) string {
	return id + "@" + version
}

func validPlatform(platform Platform) bool {
	return validIdentifier(platform.OS) && validIdentifier(platform.Arch)
}

func validSourceCatalog(value string) bool {
	if validIdentifier(value) {
		return true
	}
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(value) > maxIdentifierBytes {
		return false
	}
	for _, part := range parts {
		if !validIdentifier(part) {
			return false
		}
	}
	return true
}

func validIdentifier(value string) bool {
	if len(value) == 0 || len(value) > maxIdentifierBytes || value != strings.ToLower(value) {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '-' || r == '_' || r == '.')) {
			continue
		}
		return false
	}
	return true
}

func validExactText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
