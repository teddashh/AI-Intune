package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
)

func TestBuiltinProtectMatchesScript(t *testing.T) {
	script := string(ScriptBytes())
	ops, err := os.ReadFile(filepath.Join("..", "..", "ops", "maintenance", "disk-clean"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ops) != script {
		t.Fatal("embedded disk-clean differs from ops/maintenance/disk-clean")
	}
	const key = "BUILTIN_PROTECT='"
	i := strings.Index(script, key)
	if i < 0 {
		t.Fatal("script has no BUILTIN_PROTECT")
	}
	rest := script[i+len(key):]
	j := strings.Index(rest, "'")
	if j < 0 {
		t.Fatal("BUILTIN_PROTECT is unterminated")
	}
	got := strings.Fields(rest[:j])
	if strings.Join(got, " ") != BuiltinProtectLine() {
		t.Fatalf("script builtin\n%s\ngo\n%s", strings.Join(got, " "), BuiltinProtectLine())
	}
	if len(BuiltinProtect) < 20 {
		t.Fatalf("protected list shrank to %d", len(BuiltinProtect))
	}
}

func TestRenderDigestMatchesScript(t *testing.T) {
	p := exampleUser()
	conf, digest, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(conf)
	if digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s is not sha256 of the rendered bytes", digest)
	}
	if !strings.Contains(string(conf), "PROTECT_NAMES=\""+BuiltinProtectLine()+"\"") {
		t.Fatal("rendered conf dropped a built-in protect name")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "disk-clean.conf")
	if err := os.WriteFile(path, conf, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", scriptPath(t), "--scope", "user", "--conf", path, "--print-config").CombinedOutput()
	if err != nil {
		t.Fatalf("print-config: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "digest="+digest) {
		t.Fatalf("script digest is not the rendered digest\n%s", out)
	}
	again, digest2, err := Render(p)
	if err != nil || string(again) != string(conf) || digest2 != digest {
		t.Fatal("render is not deterministic")
	}
}

func TestConfigCannotShrinkBuiltinProtect(t *testing.T) {
	for _, protect := range []string{"omit", "zzz-only"} {
		t.Run(protect, func(t *testing.T) {
			dir := t.TempDir()
			tmp := filepath.Join(dir, "tmp")
			state := filepath.Join(dir, "state")
			if err := os.MkdirAll(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-10 * 24 * time.Hour)
			mustFile(t, filepath.Join(tmp, "safe-old.txt"), old)
			for _, name := range []string{"openclaw-state", "my-backup", "agent.sock", "run.pid", "run.lock", "systemd-private-xyz", "odoo-cache", "postgres-x", ".s.PGSQL.5432"} {
				mustFile(t, filepath.Join(tmp, name), old)
			}
			mustFile(t, filepath.Join(tmp, "recent.txt"), time.Now())
			var body strings.Builder
			body.WriteString("DRY_RUN=0\nCATEGORIES=\"user_tmp\"\nTMP_AGE_DAYS=1\nMOUNT=/\n")
			body.WriteString("TMP_DIRS=\"" + tmp + "\"\n")
			if protect != "omit" {
				body.WriteString("PROTECT_NAMES=\"" + protect + "\"\n")
			}
			conf := filepath.Join(dir, "c.conf")
			if err := os.WriteFile(conf, []byte(body.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", scriptPath(t), "--scope", "user", "--conf", conf)
			cmd.Env = append(os.Environ(), "HOME="+dir, "DISK_CLEAN_STATE="+state)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("disk-clean: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(tmp, "safe-old.txt")); !os.IsNotExist(err) {
				t.Fatal("eligible file was not removed")
			}
			for _, name := range []string{"openclaw-state", "my-backup", "agent.sock", "run.pid", "run.lock", "systemd-private-xyz", "odoo-cache", "postgres-x", ".s.PGSQL.5432", "recent.txt"} {
				if _, err := os.Stat(filepath.Join(tmp, name)); err != nil {
					t.Fatalf("%s was not protected (%s): %v", name, protect, err)
				}
			}
			line := lastJSONLine(t, out)
			summary, err := ParseSummary(line)
			if err != nil {
				t.Fatal(err)
			}
			if summary.ConfigDigest != Digest(mustRead(t, conf)) {
				t.Fatalf("summary digest %s != file", summary.ConfigDigest)
			}
		})
	}
}

func TestScriptRefusesProtectedGlob(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Mkdir(filepath.Join(tmp, "node-old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(tmp, "node-old"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tmp, "cache-backup-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(tmp, "cache-backup-1"), old, old); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "c.conf")
	body := "DRY_RUN=0\nCATEGORIES=\"tmp_globs\"\nTMP_AGE_DAYS=1\nMOUNT=/\nTMP_DIRS=\"" + tmp + "\"\n" +
		"TMP_GLOB_RULE=" + tmp + "/node-*|1|0|\n" +
		"TMP_GLOB_RULE=" + tmp + "/*backup*|1|0|\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", scriptPath(t), "--scope", "user", "--conf", conf)
	cmd.Env = append(os.Environ(), "HOME="+dir, "DISK_CLEAN_STATE="+state)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("disk-clean: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "node-old")); !os.IsNotExist(err) {
		t.Fatal("eligible glob directory was not removed")
	}
	if _, err := os.Stat(filepath.Join(tmp, "cache-backup-1")); err != nil {
		t.Fatal("protected glob directory was removed")
	}
	summary, err := ParseSummary(lastJSONLine(t, out))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.Categories["tmp_globs"].Note, "refused:") {
		t.Fatalf("note = %q", summary.Categories["tmp_globs"].Note)
	}
}

func TestProfileValidationRefusesProtectedAndUnknown(t *testing.T) {
	p := exampleUser()
	p.TmpGlobRules = []TmpGlobRule{{Glob: "/tmp/*backup*", MinAgeDays: 7, KeepNewest: 1}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "targets protected data") {
		t.Fatalf("err = %v", err)
	}
	p = exampleUser()
	p.TmpGlobRules = []TmpGlobRule{{Glob: "/tmp/*", MinAgeDays: 7, KeepNewest: 1}}
	if err := p.Validate(); err == nil {
		t.Fatal("catch-all glob was accepted")
	}
	p = exampleUser()
	p.TmpDirs = []string{"/tmp", "/home/agent"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "tmp_dirs") {
		t.Fatalf("err = %v", err)
	}
	p = exampleUser()
	p.Categories = []string{"docker_volume"}
	if err := p.Validate(); err == nil {
		t.Fatal("docker volume category was accepted")
	}
	raw, err := json.Marshal(exampleUser())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["docker_prune"] = "volumes"
	doc["protect_names"] = "only-this"
	extra, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProfile(extra); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v", err)
	}
	p = exampleUser()
	p.ExtraProtectNames = []string{"custom-temp", "*backup*"}
	conf, _, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	protectLine := ""
	for _, line := range strings.Split(string(conf), "\n") {
		if strings.HasPrefix(line, "PROTECT_NAMES=") {
			protectLine = line
		}
	}
	if !strings.Contains(protectLine, "custom-temp") || strings.Count(protectLine, "*backup*") != 1 {
		t.Fatalf("extras were not additive: %s", protectLine)
	}
	spec, conf2, err := BuildSpec(p)
	if err != nil || string(conf2) != string(conf) {
		t.Fatal(err)
	}
	rawSpec, err := MarshalSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSpec(rawSpec); err != nil {
		t.Fatal(err)
	}
	var tampered map[string]any
	if err := json.Unmarshal(rawSpec, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["command"] = "rm -rf /"
	bad, _ := json.Marshal(tampered)
	if _, err := ParseSpec(bad); err == nil {
		t.Fatal("spec accepted an operator command")
	}
}

func TestSummaryAndVerdict(t *testing.T) {
	line := summaryLine(t, "apply", Digest([]byte("x")), "category errors: npm_cache")
	s, err := ParseSummary([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSummary([]byte(line + "\n" + line)); err == nil {
		t.Fatal("two JSON values were accepted")
	}
	if _, err := ParseSummary([]byte(strings.Replace(line, "}", `,"command":"rm"}`, 1))); err == nil {
		t.Fatal("unknown summary field was accepted")
	}
	huge := bytesRepeat(MaxSummaryBytes + 1)
	if _, err := ParseSummary(huge); err == nil {
		t.Fatal("oversize summary was accepted")
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	diskFail := EvaluateObservationDisk(5, 100, &compliance.Rule{Kind: compliance.RuleDiskFreeMinPercent, MinFreePercent: 10})
	if diskFail.Outcome != OutcomeFail {
		t.Fatalf("disk = %+v", diskFail)
	}
	diskPass := EvaluateObservationDisk(50, 100, &compliance.Rule{Kind: compliance.RuleDiskFreeMinPercent, MinFreePercent: 10})
	if diskPass.Outcome != OutcomePass {
		t.Fatalf("disk = %+v", diskPass)
	}
	unmeasured := EvaluateObservationDisk(0, 0, nil)
	if unmeasured.Outcome != OutcomeUnmeasured {
		t.Fatalf("disk = %+v", unmeasured)
	}
	mismatch := Decide(&s, now, now, "sha256:"+strings.Repeat("ab", 32), diskPass)
	if mismatch.Outcome != OutcomeFail || mismatch.DigestOK {
		t.Fatalf("verdict = %+v", mismatch)
	}
	s.ConfigDigest = "sha256:" + strings.Repeat("ab", 32)
	s.Attention = ""
	s.Mode = "apply"
	ok := Decide(&s, now, now, s.ConfigDigest, diskPass)
	if ok.Outcome != OutcomePass {
		t.Fatalf("verdict = %+v", ok)
	}
	low := Decide(&s, now, now, s.ConfigDigest, diskFail)
	if low.Outcome != OutcomeFail {
		t.Fatalf("disk fail after apply = %+v", low)
	}
	s.Mode = "dry-run"
	s.Attention = "disk 91% >= 90% after cleaning"
	hint := Decide(&s, now, now, s.ConfigDigest, diskFail)
	if hint.Outcome != OutcomeAttention {
		t.Fatalf("dry-run disk fail must stay a hint, got %+v", hint)
	}
	stale := Decide(&s, now.Add(-49*time.Hour), now, s.ConfigDigest, diskPass)
	if !stale.Stale || stale.Outcome != OutcomeAttention {
		t.Fatalf("stale+attention = %+v", stale)
	}
	missing := Decide(nil, time.Time{}, now, s.ConfigDigest, diskPass)
	if missing.Outcome != OutcomeStale || !missing.Stale {
		t.Fatalf("missing = %+v", missing)
	}
	views := AlertViews(low)
	if !views[2].Active || views[2].Condition != AlertDiskOverThreshold {
		t.Fatalf("views = %+v", views)
	}
	dryViews := AlertViews(hint)
	if dryViews[2].Active || !dryViews[0].Active {
		t.Fatalf("dry views = %+v", dryViews)
	}
	var prev StoredAlert
	if !ShouldNotify(prev, views[2]) {
		t.Fatal("first alert was suppressed")
	}
	prev = StoredAlert{Active: true, Delivered: true, Fingerprint: views[2].Fingerprint}
	if ShouldNotify(prev, views[2]) {
		t.Fatal("same fingerprint was re-alerted")
	}
	prev.Delivered = false
	if !ShouldNotify(prev, views[2]) {
		t.Fatal("undelivered alert was not retried")
	}
	cleared := views[2]
	cleared.Active = false
	if ShouldNotify(prev, cleared) {
		t.Fatal("cleared condition notified")
	}
	prev.Active = false
	if !ShouldNotify(prev, views[2]) {
		t.Fatal("condition returning after a clear was suppressed")
	}
}

func exampleUser() Profile {
	dry := true
	npm, pip, goc := 1024, 200, 5120
	thumb, trash := 30, 30
	duT, duD := 180, 2
	return Profile{
		SchemaVersion: SchemaVersion,
		Scope:         ScopeUser,
		DryRun:        &dry,
		Categories:    []string{"user_tmp", "tmp_globs"},
		TmpDirs:       []string{"/var/tmp", "/tmp"},
		TmpAgeDays:    7,
		NpmCleanMinMB: &npm,
		PipCacheMinMB: &pip,
		GoCacheMinMB:  &goc,
		ThumbAgeDays:  &thumb,
		TrashAgeDays:  &trash,
		AttentionPct:  90,
		Mount:         "/",
		DuTimeoutS:    &duT,
		DuDepth:       &duD,
		ReportPaths:   []string{"$HOME/clawctl-backups", "$HOME/*backup*"},
	}
}

func summaryLine(t *testing.T, mode, digest, attention string) string {
	t.Helper()
	s := Summary{
		Schema: SummarySchema, Version: ScriptVersion, Scope: ScopeUser,
		Host: "host-a", User: "agent", Mode: mode,
		TS: "2026-10-06T12:00:00Z", DurationS: 1, ConfigDigest: digest,
		Disk:           SummaryDisk{Mount: "/", PctBefore: 10, PctAfter: 10, AvailBytesBefore: 1, AvailBytesAfter: 1},
		CandidateBytes: 0, FreedBytes: 0,
		Categories: map[string]SummaryCategory{
			"user_tmp": {Mode: "dry-run", Status: "ok", Note: "age>=7d"},
		},
		Attention: attention,
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func scriptPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "ops", "maintenance", "disk-clean")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustFile(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lastJSONLine(t *testing.T, out []byte) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "{") {
			return []byte(lines[i])
		}
	}
	t.Fatalf("no JSON line in %s", out)
	return nil
}

func bytesRepeat(n int) []byte {
	return []byte(strings.Repeat("x", n))
}

func TestRootScopeRejectsUserKeys(t *testing.T) {
	hours := 168
	vartmp := 14
	dry := false
	p := Profile{
		SchemaVersion: SchemaVersion, Scope: ScopeRoot, DryRun: &dry,
		Categories: []string{"journal"}, TmpAgeDays: 10, VarTmpAgeDays: &vartmp,
		JournalMaxSize: "500M", JournalMaxAge: "30d", DockerUntilHours: &hours,
		AttentionPct: 90, Mount: "/",
	}
	if _, _, err := Render(p); err != nil {
		t.Fatal(err)
	}
	p.TmpDirs = []string{"/tmp"}
	if err := p.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestExpectedApplyMode(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		p    Profile
		want string
	}{
		{Profile{DryRun: &no}, "apply"},
		{Profile{DryRun: &yes}, "dry-run"},
		{Profile{DryRun: &yes, ApplyCategories: []string{"user_tmp"}}, "mixed"},
	}
	for _, c := range cases {
		if got := c.p.ExpectedApplyMode(); got != c.want {
			t.Fatalf("ExpectedApplyMode(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

func TestObservationPredatesSummary(t *testing.T) {
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ts := base.Format(time.RFC3339)
	if !ObservationPredatesSummary(base.Add(time.Minute), base.Add(-time.Second), ts, base) {
		t.Fatal("observation received before the summary must predate it")
	}
	if !ObservationPredatesSummary(base.Add(-time.Minute), base.Add(time.Minute), ts, base) {
		t.Fatal("observation measured before the script ts must predate it")
	}
	if ObservationPredatesSummary(base.Add(time.Minute), base.Add(time.Minute), ts, base) {
		t.Fatal("a later observation is a post-clean reading")
	}
}
