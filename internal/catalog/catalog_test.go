package catalog

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	nodeDigest     = "1111111111111111111111111111111111111111111111111111111111111111"
	openClawDigest = "2222222222222222222222222222222222222222222222222222222222222222"
	hermesDigest   = "3333333333333333333333333333333333333333333333333333333333333333"
)

var linuxAMD64 = Platform{OS: "linux", Arch: "amd64"}

func manifest(id, version string, kind PackageKind, digest string) Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Version:       version,
		Kind:          kind,
		Title:         id,
		Source: Source{
			Catalog: "ai-intune", UpstreamURL: "https://github.com/example/" + id,
			Revision: version, License: "MIT",
		},
		Artifact:  Artifact{SHA256: digest, Size: 1024},
		Adapter:   Adapter{Name: id, Version: 1},
		Platforms: []Platform{linuxAMD64, {OS: "linux", Arch: "arm64"}},
		Provides:  []string{"package." + id},
		Conflicts: []string{}, ExclusiveGroups: []string{}, Dependencies: []PackageRef{},
	}
}

func standardManifests() []Manifest {
	node := manifest("node-runtime", "24.15.0", KindRuntime, nodeDigest)
	node.Provides = []string{"runtime.node"}

	openclaw := manifest("openclaw", "2026.9.2", KindApp, openClawDigest)
	openclaw.Dependencies = []PackageRef{{PackageID: node.ID, Version: node.Version}}
	openclaw.Provides = []string{CapabilityAgentRuntime}
	openclaw.Conflicts = []string{"hermes-agent"}
	openclaw.ExclusiveGroups = []string{ExclusiveGroupPrimaryAgentRuntime}

	hermes := manifest("hermes-agent", "1.0.0", KindApp, hermesDigest)
	hermes.Provides = []string{CapabilityAgentRuntime}
	hermes.Conflicts = []string{"openclaw"}
	hermes.ExclusiveGroups = []string{ExclusiveGroupPrimaryAgentRuntime}
	return []Manifest{openclaw, hermes, node}
}

func profile(refs ...PackageRef) MachineProfile {
	return MachineProfile{SchemaVersion: SchemaVersion, ID: "standard", Revision: 7, Packages: refs}
}

func ref(m Manifest) PackageRef {
	return PackageRef{PackageID: m.ID, Version: m.Version}
}

func TestParseManifestAcceptsOnlyTheExactCompleteSchema(t *testing.T) {
	valid, err := json.Marshal(standardManifests()[0])
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(valid)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if parsed.ID != "openclaw" || parsed.Source.Catalog != "ai-intune" {
		t.Fatalf("manifest decoded incorrectly: %+v", parsed)
	}
	canonical, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(canonical); err != nil {
		t.Fatalf("canonical manifest cannot be parsed again: %v\n%s", err, canonical)
	}

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"not object", `[]`, "JSON object"},
		{"unknown top field", strings.TrimSuffix(string(valid), "}") + `,"note":"x"}`, "unknown field"},
		{"case alias", strings.Replace(string(valid), `"schema_version":`, `"SchemaVersion":`, 1), "unknown field"},
		{"duplicate nested field", strings.Replace(string(valid), `"catalog":"ai-intune"`, `"catalog":"ai-intune","catalog":"other"`, 1), "duplicate object field"},
		{"missing required field", strings.Replace(string(valid), `"conflicts":["hermes-agent"],`, "", 1), "missing field"},
		{"null list", strings.Replace(string(valid),
			`"platforms":[{"os":"linux","arch":"amd64"},{"os":"linux","arch":"arm64"}]`,
			`"platforms":null`, 1), "null is not allowed"},
		{"trailing document", string(valid) + `{}`, "trailing JSON"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestParseProfileSortsExactSelectionsAndRejectsAmbiguousJSON(t *testing.T) {
	raw := []byte(`{"schema_version":1,"id":"agents","revision":3,"packages":[` +
		`{"package_id":"openclaw","version":"2026.9.2"},` +
		`{"package_id":"node-runtime","version":"24.15.0"}]}`)
	parsed, err := ParseProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Packages[0].PackageID; got != "node-runtime" {
		t.Fatalf("profile selections not canonical: %+v", parsed.Packages)
	}
	for name, bad := range map[string]string{
		"missing revision": strings.Replace(string(raw), `"revision":3,`, "", 1),
		"case alias":       strings.Replace(string(raw), `"packages":`, `"Packages":`, 1),
		"duplicate id":     strings.Replace(string(raw), `"id":"agents"`, `"id":"agents","id":"other"`, 1),
		"null packages": strings.Replace(string(raw),
			`"packages":[{"package_id":"openclaw","version":"2026.9.2"},{"package_id":"node-runtime","version":"24.15.0"}]`,
			`"packages":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProfile([]byte(bad)); err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
}

func TestOpenClawProfileResolvesRuntimeBeforeApp(t *testing.T) {
	manifests := standardManifests()
	c, err := New(manifests)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.Resolve(profile(ref(manifests[0])), linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProfileID != "standard" || plan.ProfileRevision != 7 || plan.Target != linuxAMD64 {
		t.Fatalf("plan identity incorrect: %+v", plan)
	}
	if len(plan.Packages) != 2 {
		t.Fatalf("plan packages=%d want=2", len(plan.Packages))
	}
	if got := plan.Packages[0]; got.Manifest.ID != "node-runtime" || got.Direct ||
		!reflect.DeepEqual(got.RequiredBy, []string{"openclaw@2026.9.2"}) {
		t.Fatalf("runtime plan entry incorrect: %+v", got)
	}
	if got := plan.Packages[1]; got.Manifest.ID != "openclaw" || !got.Direct || len(got.RequiredBy) != 0 {
		t.Fatalf("app plan entry incorrect: %+v", got)
	}
}

func TestResolutionIsStableAndSharedDependenciesAppearOnce(t *testing.T) {
	manifests := standardManifests()
	worker := manifest("worker-ui", "2.0.0", KindApp, "4444444444444444444444444444444444444444444444444444444444444444")
	worker.Dependencies = []PackageRef{{PackageID: "node-runtime", Version: "24.15.0"}}
	manifests = append(manifests, worker)

	first, err := New(manifests)
	if err != nil {
		t.Fatal(err)
	}
	reversed := append([]Manifest(nil), manifests...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	second, err := New(reversed)
	if err != nil {
		t.Fatal(err)
	}
	openclaw := ref(manifests[0])
	workerRef := ref(worker)
	planA, err := first.Resolve(profile(workerRef, openclaw), linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	planB, err := second.Resolve(profile(openclaw, workerRef), linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	identities := func(plan Plan) []string {
		out := make([]string, 0, len(plan.Packages))
		for _, item := range plan.Packages {
			out = append(out, packageIdentity(item.Manifest.ID, item.Manifest.Version))
		}
		return out
	}
	want := []string{"node-runtime@24.15.0", "openclaw@2026.9.2", "worker-ui@2.0.0"}
	if got := identities(planA); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan A=%v want=%v", got, want)
	}
	if got := identities(planB); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan B=%v want=%v", got, want)
	}
	if got := planA.Packages[0].RequiredBy; !reflect.DeepEqual(got, []string{"openclaw@2026.9.2", "worker-ui@2.0.0"}) {
		t.Fatalf("shared dependency parents=%v", got)
	}
}

func TestPrimaryAgentRuntimeIsSelectOne(t *testing.T) {
	manifests := standardManifests()
	c, err := New(manifests)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Resolve(profile(ref(manifests[0]), ref(manifests[1])), linuxAMD64)
	var resolution *ResolutionError
	if !errors.As(err, &resolution) || resolution.Code != CodeExclusiveGroupConflict ||
		resolution.Group != ExclusiveGroupPrimaryAgentRuntime {
		t.Fatalf("error=%+v want %s", err, CodeExclusiveGroupConflict)
	}
}

func TestAsymmetricPackageConflictStillBlocksPlan(t *testing.T) {
	left := manifest("left", "1", KindApp, nodeDigest)
	right := manifest("right", "1", KindApp, openClawDigest)
	left.Conflicts = []string{"right"}
	c, err := New([]Manifest{right, left})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Resolve(profile(ref(right), ref(left)), linuxAMD64)
	var resolution *ResolutionError
	if !errors.As(err, &resolution) || resolution.Code != CodePackageConflict {
		t.Fatalf("error=%+v want %s", err, CodePackageConflict)
	}
}

func TestMissingVersionCycleAndPlatformHaveDistinctCodes(t *testing.T) {
	tests := []struct {
		name     string
		catalog  []Manifest
		profile  MachineProfile
		target   Platform
		wantCode ResolutionCode
	}{
		{
			name: "missing dependency",
			catalog: func() []Manifest {
				app := manifest("app", "1", KindApp, openClawDigest)
				app.Dependencies = []PackageRef{{PackageID: "missing", Version: "1"}}
				return []Manifest{app}
			}(),
			profile: profile(PackageRef{PackageID: "app", Version: "1"}), target: linuxAMD64,
			wantCode: CodeMissingPackage,
		},
		{
			name: "version conflict",
			catalog: func() []Manifest {
				app := manifest("app", "1", KindApp, openClawDigest)
				app.Dependencies = []PackageRef{{PackageID: "node", Version: "24"}}
				return []Manifest{
					app,
					manifest("node", "22", KindRuntime, nodeDigest),
					manifest("node", "24", KindRuntime, hermesDigest),
				}
			}(),
			profile: profile(PackageRef{PackageID: "node", Version: "22"}, PackageRef{PackageID: "app", Version: "1"}), target: linuxAMD64,
			wantCode: CodeVersionConflict,
		},
		{
			name: "dependency cycle",
			catalog: func() []Manifest {
				a := manifest("a", "1", KindRuntime, nodeDigest)
				b := manifest("b", "1", KindRuntime, openClawDigest)
				a.Dependencies = []PackageRef{{PackageID: "b", Version: "1"}}
				b.Dependencies = []PackageRef{{PackageID: "a", Version: "1"}}
				return []Manifest{a, b}
			}(),
			profile: profile(PackageRef{PackageID: "a", Version: "1"}), target: linuxAMD64,
			wantCode: CodeDependencyCycle,
		},
		{
			name: "unsupported platform",
			catalog: []Manifest{func() Manifest {
				app := manifest("app", "1", KindApp, openClawDigest)
				app.Platforms = []Platform{{OS: "linux", Arch: "arm64"}}
				return app
			}()},
			profile: profile(PackageRef{PackageID: "app", Version: "1"}), target: linuxAMD64,
			wantCode: CodeUnsupportedPlatform,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, err := New(test.catalog)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Resolve(test.profile, test.target)
			var resolution *ResolutionError
			if !errors.As(err, &resolution) || resolution.Code != test.wantCode {
				t.Fatalf("error=%+v want code %s", err, test.wantCode)
			}
		})
	}
}

func TestCatalogOwnsCanonicalCopies(t *testing.T) {
	app := manifest("app", "1", KindApp, openClawDigest)
	app.Provides = []string{"z", "a"}
	c, err := New([]Manifest{app})
	if err != nil {
		t.Fatal(err)
	}
	app.Provides[0] = "mutated"
	plan, err := c.Resolve(profile(PackageRef{PackageID: "app", Version: "1"}), linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Packages[0].Manifest.Provides; !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("catalog changed through caller alias: %v", got)
	}
	plan.Packages[0].Manifest.Provides[0] = "mutated-output"
	again, err := c.Resolve(profile(PackageRef{PackageID: "app", Version: "1"}), linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Packages[0].Manifest.Provides; !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("catalog changed through plan alias: %v", got)
	}
}

func TestManifestValidationRejectsNonDeployableRecords(t *testing.T) {
	base := manifest("app", "1", KindApp, openClawDigest)
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"uppercase id", func(m *Manifest) { m.ID = "OpenClaw" }, "id"},
		{"mutable source URL", func(m *Manifest) { m.Source.UpstreamURL += "?token=x" }, "upstream_url"},
		{"bad digest", func(m *Manifest) { m.Artifact.SHA256 = strings.Repeat("A", 64) }, "sha256"},
		{"zero adapter version", func(m *Manifest) { m.Adapter.Version = 0 }, "adapter version"},
		{"no platform", func(m *Manifest) { m.Platforms = nil }, "platform count"},
		{"duplicate capability", func(m *Manifest) { m.Provides = []string{"x", "x"} }, "duplicate"},
		{"self conflict", func(m *Manifest) { m.Conflicts = []string{"app"} }, "own id"},
		{"self dependency", func(m *Manifest) { m.Dependencies = []PackageRef{{PackageID: "app", Version: "1"}} }, "itself"},
		{"agent runtime without slot", func(m *Manifest) { m.Provides = []string{CapabilityAgentRuntime} }, ExclusiveGroupPrimaryAgentRuntime},
		{"agent slot without capability", func(m *Manifest) { m.ExclusiveGroups = []string{ExclusiveGroupPrimaryAgentRuntime} }, CapabilityAgentRuntime},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			if err := ValidateManifest(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}

	if _, err := New([]Manifest{base, base}); err == nil || !strings.Contains(err.Error(), "duplicate package") {
		t.Fatalf("duplicate package error=%v", err)
	}
}
