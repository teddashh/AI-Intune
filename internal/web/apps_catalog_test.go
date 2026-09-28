package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

func writeCatalogWebArtifact(t *testing.T, dir, name, version, engines string) artifact.Sidecar {
	t.Helper()
	body := []byte("catalog web artifact " + name + " " + version)
	if name == "node-runtime" {
		var bundle bytes.Buffer
		gz := gzip.NewWriter(&bundle)
		tw := tar.NewWriter(gz)
		for _, target := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
			for _, entry := range []struct {
				name string
				mode int64
				body string
			}{
				{name: "node-runtime/" + target + "/bin/node", mode: 0o755, body: "node-" + target},
				{name: "node-runtime/" + target + "/lib/node_modules/npm/bin/npm-cli.js", mode: 0o644, body: "npm-" + target},
			} {
				if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg,
					Mode: entry.mode, Size: int64(len(entry.body))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write([]byte(entry.body)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		body = bundle.Bytes()
	}
	sha, integrity := sha256.Sum256(body), sha512.Sum512(body)
	record := artifact.Sidecar{
		Name: name, Version: version, SHA256: hex.EncodeToString(sha[:]), Size: int64(len(body)),
		TarballURL:      "https://private-source.invalid/SECRET_CATALOG_WEB_SOURCE/" + name + ".tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(integrity[:]),
		EnginesNode:     engines, FetchedAt: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		FetchedBy: "SECRET_CATALOG_WEB_ACTOR",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestCatalogWebStandardStoreProfileAndAssignment(t *testing.T) {
	s, st := newServer(t)
	dir := t.TempDir()
	node := writeCatalogWebArtifact(t, dir, "node-runtime", "24.21.0", "")
	openclaw := writeCatalogWebArtifact(t, dir, "openclaw", "2026.9.2", ">=24.15.0 <25")
	s.SetArtifactsDir(dir)
	machineID := onlineMachine(t, st, "catalog-web-target")
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}

	storePage := renderWithCapabilities(t, s, "/apps?view=store", names)
	for _, want := range []string{"Standard Store", "Add package", "node-runtime@24.21.0", "openclaw@2026.9.2"} {
		if !strings.Contains(storePage, want) {
			t.Errorf("Store page missing %q", want)
		}
	}
	for _, private := range []string{"SECRET_CATALOG_WEB_SOURCE", "SECRET_CATALOG_WEB_ACTOR", dir} {
		if strings.Contains(storePage, private) {
			t.Errorf("Store page disclosed %q", private)
		}
	}

	nodePreview := postForm(t, s, "/apps/store/packages/preview", url.Values{
		"artifact_sha256": {node.SHA256}, "node_runtime_version": {""}, "reason": {"publish runtime"},
	})
	if nodePreview.Code != http.StatusOK || !strings.Contains(nodePreview.Body.String(), "確認 Store package") ||
		!strings.Contains(nodePreview.Body.String(), "node-runtime@24.21.0") {
		t.Fatalf("node preview status=%d body=%s", nodePreview.Code, nodePreview.Body.String())
	}
	nodeForm := url.Values{
		"manifest":        {hiddenFormValue(t, nodePreview.Body.String(), "manifest")},
		"preview_digest":  {hiddenFormValue(t, nodePreview.Body.String(), "preview_digest")},
		"reason":          {hiddenFormValue(t, nodePreview.Body.String(), "reason")},
		"idempotency_key": {hiddenFormValue(t, nodePreview.Body.String(), "idempotency_key")},
		"confirm_package": {"node-runtime@24.21.0"},
	}
	nodePublished := postForm(t, s, "/apps/store/packages", nodeForm)
	nodeReplay := postForm(t, s, "/apps/store/packages", nodeForm)
	if nodePublished.Code != http.StatusSeeOther || nodePublished.Header().Get("Location") != "/apps?view=store" ||
		nodeReplay.Code != http.StatusSeeOther || nodeReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("node publish=%d replay=%d replay_headers=%v", nodePublished.Code, nodeReplay.Code, nodeReplay.Header())
	}

	openPreview := postForm(t, s, "/apps/store/packages/preview", url.Values{
		"artifact_sha256": {openclaw.SHA256}, "node_runtime_version": {node.Version}, "reason": {"publish agent"},
	})
	if openPreview.Code != http.StatusOK || !strings.Contains(openPreview.Body.String(), "openclaw@2026.9.2") ||
		!strings.Contains(openPreview.Body.String(), "node-runtime@24.21.0") ||
		!strings.Contains(openPreview.Body.String(), "hermes-agent") {
		t.Fatalf("OpenClaw preview status=%d body=%s", openPreview.Code, openPreview.Body.String())
	}
	openPublished := postForm(t, s, "/apps/store/packages", url.Values{
		"manifest":        {hiddenFormValue(t, openPreview.Body.String(), "manifest")},
		"preview_digest":  {hiddenFormValue(t, openPreview.Body.String(), "preview_digest")},
		"reason":          {hiddenFormValue(t, openPreview.Body.String(), "reason")},
		"idempotency_key": {hiddenFormValue(t, openPreview.Body.String(), "idempotency_key")},
		"confirm_package": {"openclaw@2026.9.2"},
	})
	if openPublished.Code != http.StatusSeeOther {
		t.Fatalf("OpenClaw publish status=%d body=%s", openPublished.Code, openPublished.Body.String())
	}

	profilesPage := renderWithCapabilities(t, s, "/apps?view=profiles", names)
	if !strings.Contains(profilesPage, `action="/apps/profiles/preview"`) || !strings.Contains(profilesPage, "openclaw@2026.9.2") {
		t.Fatalf("profiles page=%s", profilesPage)
	}
	profilePreview := postForm(t, s, "/apps/profiles/preview", url.Values{
		"profile_id": {"openclaw-standard"}, "revision": {"1"},
		"package": {"openclaw@2026.9.2"}, "reason": {"publish managed profile"},
	})
	if profilePreview.Code != http.StatusOK || !strings.Contains(profilePreview.Body.String(), "openclaw-standard@1") {
		t.Fatalf("profile preview status=%d body=%s", profilePreview.Code, profilePreview.Body.String())
	}
	profilePublished := postForm(t, s, "/apps/profiles", url.Values{
		"profile":         {hiddenFormValue(t, profilePreview.Body.String(), "profile")},
		"preview_digest":  {hiddenFormValue(t, profilePreview.Body.String(), "preview_digest")},
		"reason":          {hiddenFormValue(t, profilePreview.Body.String(), "reason")},
		"idempotency_key": {hiddenFormValue(t, profilePreview.Body.String(), "idempotency_key")},
		"confirm_profile": {"openclaw-standard@1"},
	})
	if profilePublished.Code != http.StatusSeeOther || profilePublished.Header().Get("Location") != "/apps?view=profiles" {
		t.Fatalf("profile publish status=%d body=%s", profilePublished.Code, profilePublished.Body.String())
	}

	// ⚠⚠ 這就是正式庫 2026-09-12 的形狀：發佈了一版、點名了一個套件、而一台都沒指派。
	// 這一頁原本完全講不出這件事——它只列出「有這一版」。
	published := html.UnescapeString(renderWithCapabilities(t, s, "/apps?view=profiles", names))
	for _, want := range []string{
		"openclaw-standard@1",
		operator.ProfileStateTitle(operator.ProfileUnassigned),
		operator.ProfileStateNextStep(operator.ProfileUnassigned),
		"0 / 1 台",
		operator.ProfileReportCaveat,
	} {
		if !strings.Contains(published, want) {
			t.Errorf("發佈之後那一頁少了 %q", want)
		}
	}
	if strings.Contains(published, operator.ProfileStateTitle(operator.ProfileInUse)) {
		t.Errorf("一台都沒指派卻說機隊上有機器穿著這一版")
	}

	assignmentPage := renderWithCapabilities(t, s, "/apps?view=assignments", names)
	if !strings.Contains(assignmentPage, "catalog-web-target") || !strings.Contains(assignmentPage, "openclaw-standard@1") {
		t.Fatalf("assignment page=%s", assignmentPage)
	}
	assignmentPreview := postForm(t, s, "/apps/profile-assignments/preview", url.Values{
		"machine_id": {machineID}, "profile": {"openclaw-standard@1"}, "reason": {"apply managed profile"},
	})
	if assignmentPreview.Code != http.StatusOK || !strings.Contains(assignmentPreview.Body.String(), "建立工作單") ||
		!strings.Contains(assignmentPreview.Body.String(), "node-runtime@24.21.0") ||
		!strings.Contains(assignmentPreview.Body.String(), "openclaw@2026.9.2") ||
		!strings.Contains(assignmentPreview.Body.String(), "Create assignment and jobs") {
		t.Fatalf("assignment preview status=%d body=%s", assignmentPreview.Code, assignmentPreview.Body.String())
	}
	assigned := postForm(t, s, "/apps/profile-assignments", url.Values{
		"machine_id":       {machineID},
		"profile_id":       {hiddenFormValue(t, assignmentPreview.Body.String(), "profile_id")},
		"profile_revision": {hiddenFormValue(t, assignmentPreview.Body.String(), "profile_revision")},
		"preview_digest":   {hiddenFormValue(t, assignmentPreview.Body.String(), "preview_digest")},
		"reason":           {hiddenFormValue(t, assignmentPreview.Body.String(), "reason")},
		"idempotency_key":  {hiddenFormValue(t, assignmentPreview.Body.String(), "idempotency_key")},
		"confirm_name":     {"catalog-web-target"},
	})
	if assigned.Code != http.StatusSeeOther || assigned.Header().Get("Location") != "/machines/"+machineID {
		t.Fatalf("assignment status=%d headers=%v body=%s", assigned.Code, assigned.Header(), assigned.Body.String())
	}
	var jobs int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE machine_id=?`, machineID).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("assigned jobs=%d err=%v", jobs, err)
	}

	// ⚠ 指派完兩頁都要換答案：Profiles 那一版變成「機隊上有機器穿著這一版」，
	// Assignments 那一台講得出它現在穿的是哪一版。
	wearing := html.UnescapeString(renderWithCapabilities(t, s, "/apps?view=profiles", names))
	for _, want := range []string{
		operator.ProfileStateTitle(operator.ProfileInUse), "1 / 1 台",
	} {
		if !strings.Contains(wearing, want) {
			t.Errorf("指派之後 Profiles 那一頁少了 %q", want)
		}
	}
	if strings.Contains(wearing, operator.ProfileStateTitle(operator.ProfileUnassigned)) {
		t.Errorf("已經指派了卻還說一台都沒指派")
	}
	worn := html.UnescapeString(renderWithCapabilities(t, s, "/apps?view=assignments", names))
	for _, want := range []string{
		"catalog-web-target", "openclaw-standard@1", "1 / 1 台身上有一份 profile",
	} {
		if !strings.Contains(worn, want) {
			t.Errorf("指派之後 Assignments 那一頁少了 %q", want)
		}
	}
	if strings.Contains(worn, "還沒有被指派過任何 profile") {
		t.Errorf("已經指派了卻還說這台身上沒有 profile")
	}

	// ⚠⚠ 退役之後那一版要換成「只有已退役的機器身上還是這一版」，而且那幾台的台數要
	// 留在畫面上。把它折回「一台都沒指派」的話，一份其實還掛在退役機器身上的 profile
	// 會看起來像從來沒有人用過——而那兩件事要做的處理不一樣。
	if err := st.RetireMachine(machineID, time.Now().UTC()); err != nil {
		t.Fatalf("退役: %v", err)
	}
	retired := html.UnescapeString(renderWithCapabilities(t, s, "/apps?view=profiles", names))
	for _, want := range []string{
		operator.ProfileStateTitle(operator.ProfileRetiredOnly),
		operator.ProfileStateNextStep(operator.ProfileRetiredOnly),
		"另有 1 台已退役仍是這一版",
	} {
		if !strings.Contains(retired, want) {
			t.Errorf("退役之後那一頁少了 %q", want)
		}
	}
	if strings.Contains(retired, operator.ProfileStateTitle(operator.ProfileUnassigned)) {
		t.Errorf("退役機器身上還有這一版，卻被講成一台都沒指派")
	}
}

// ⚠⚠ 這一頁原本只列得出「可以指派給誰」。一張列著五台機器的表看起來像五台都被指派了，
// 而帳本上一列指派都沒有——這兩件事在畫面上長得一模一樣，所以「現在誰有」必須是自己的
// 一欄，而「還沒有被指派過」必須是一個講出來的答案，不是一個空格。
func TestTheAssignmentsPageSaysWhichMachinesWearNothing(t *testing.T) {
	s, st := newServer(t)
	onlineMachine(t, st, "bare-target")
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	page := html.UnescapeString(renderWithCapabilities(t, s, "/apps?view=assignments", names))
	for _, want := range []string{
		"bare-target", "還沒有被指派過任何 profile", "0 / 1 台身上有一份 profile",
		"要讓其餘 1 台照一份固定的軟體集合走", operator.ProfileReportCaveat,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("那一頁少了 %q", want)
		}
	}
	// ⚠ 一版都沒發佈的時候，指派表單不可以畫出來：一個選不到任何 Profile 的下拉，
	// 按下去只會得到一個錯誤，而畫面在按之前就知道那件事。
	if !strings.Contains(page, "需要至少一台 active 機器與一個 Profile。") {
		t.Errorf("一版都沒發佈，指派表單卻還畫出來：%s", page)
	}
	profiles := html.UnescapeString(renderWithCapabilities(t, s, "/apps?view=profiles", names))
	if !strings.Contains(profiles, "還沒有 Profile。") {
		t.Errorf("一版都沒發佈，那一頁沒有說出來：%s", profiles)
	}
}

// ⚠⚠ 這兩頁與 /reports/profile 讀的必須是同一份投影。各自算一次的話，漂掉的樣子是
// 同一版 profile 在兩頁上有兩個「穿在幾台身上」，而那兩個數字沒有一個問得出誰是對的。
func TestTheAppsPagesAndTheProfileReportAgreeOnTheDenominator(t *testing.T) {
	s, st := newServer(t)
	onlineMachine(t, st, "agree-one")
	onlineMachine(t, st, "agree-two")
	// ⚠ 已退役那台不在分母裡，也不可以出現在指派表單上：把一份 profile 指派給一台
	// 已經離開機隊的機器，開出來的工作單永遠不會有人去領。
	gone := onlineMachine(t, st, "agree-retired")
	if err := st.RetireMachine(gone, time.Now().UTC()); err != nil {
		t.Fatalf("退役: %v", err)
	}
	now := time.Now().UTC()
	report, err := s.operator.ProfileReport(now)
	if err != nil {
		t.Fatal(err)
	}
	assignments, err := s.catalogAssignmentsSurface(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments.Machines) != report.Machines || report.Machines != 2 {
		t.Fatalf("表上 %d 列，報告的分母是 %d 台", len(assignments.Machines), report.Machines)
	}
	for _, machine := range assignments.Machines {
		if machine.MachineID == gone {
			t.Errorf("已退役的機器出現在指派表上")
		}
	}
	wearing := 0
	for _, machine := range assignments.Machines {
		if machine.Profile != nil {
			wearing++
		}
	}
	if wearing != report.Wearing {
		t.Errorf("表上 %d 台身上有 profile，報告說 %d 台", wearing, report.Wearing)
	}
	if len(assignments.Machines)-wearing != report.Bare {
		t.Errorf("表上 %d 台身上沒有，報告說 %d 台",
			len(assignments.Machines)-wearing, report.Bare)
	}
	profiles, err := s.catalogProfilesSurface(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles.Report.Profiles) != report.Published {
		t.Errorf("Profiles 那一頁列了 %d 版，報告說發佈了 %d 版",
			len(profiles.Report.Profiles), report.Published)
	}
}

// ⚠ 一台機器只能穿一版。反過來的報告會讓畫面顯示一個隨讀取順序改變的答案，所以這裡
// 當場拒絕，不是靜靜留下後掃到的那一版。
func TestAMachineWearingTwoRevisionsIsRefusedRatherThanShownOnce(t *testing.T) {
	machine := operator.ProfileMachineRef{MachineID: "m1", DisplayName: "samplehub1"}
	report := operator.ProfileReport{Profiles: []operator.ProfileRow{
		{ProfileID: "openclaw-standard", Revision: 1, Machines: []operator.ProfileMachineRef{machine}},
		{ProfileID: "openclaw-standard", Revision: 2, Machines: []operator.ProfileMachineRef{machine}},
	}}
	if _, err := profileWornByMachine(report); err == nil {
		t.Fatal("一台穿著兩版被接受了")
	}
	// 已退役那幾台不算「現在誰有」——它們不在分母裡，而報告照樣列出它們。
	retired := machine
	retired.Retired = true
	report.Profiles[1].Machines = []operator.ProfileMachineRef{retired}
	worn, err := profileWornByMachine(report)
	if err != nil {
		t.Fatalf("一台在籍加一台退役被拒絕了：%v", err)
	}
	if len(worn) != 1 || worn["m1"].Revision != 1 {
		t.Fatalf("worn=%+v", worn)
	}
}

func TestCatalogWebHidesAdminFormsFromViewOnlyOperator(t *testing.T) {
	s, _ := newServer(t)
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"store", "profiles", "assignments"} {
		body := renderWithCapabilities(t, s, "/apps?view="+view, operatorauth.CapabilityNames{View: names.View})
		for _, action := range []string{"/apps/store/packages/preview", "/apps/profiles/preview", "/apps/profile-assignments/preview"} {
			if strings.Contains(body, `action="`+action+`"`) {
				t.Errorf("view-only %s exposed %s", view, action)
			}
		}
	}
}
