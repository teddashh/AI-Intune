package operator

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// profileFleet 接在 installFixture 那個機隊上：同樣五台在分母裡、同樣的安裝意圖與
// 觀測，另外加上「發佈了什麼」與「誰身上現在是哪一份」。
//
// ⚠ 兩份報告共用同一個機隊不是省事，是這一頁的判準：「這個 Hub 指派過這一版沒有」
// 與每機安裝狀態讀的是同一條 desired_state，「看得到這一版沒有」讀的是同一份
// FleetTools。兩邊各造一個機隊的話，測出來的一致性是假的。
type profileFleet struct {
	installFleet
	nextAssignment int64
}

// profileManifest 發佈一份目錄 manifest。profile 只能點名目錄上有的套件版本，
// 所以每一個要被點名的版本都得先在這裡存在。
//
// ⚠ 走 Store 而不是 Service.PublishCatalogManifest：那一條要 artifact sidecar 與
// 一次操作員確認，而這裡要測的是讀的人看到什麼。
func (f *profileFleet) manifest(t *testing.T, id, version, digest string) {
	t.Helper()
	if _, err := f.store.PublishCatalogManifest(appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion, ID: id, Version: version,
		Kind: appcatalog.KindApp, Title: id,
		Source: appcatalog.Source{
			Catalog: "ai-intune", UpstreamURL: "https://example.invalid/" + id,
			Revision: version, License: "MIT",
		},
		Artifact:     appcatalog.Artifact{SHA256: digest, Size: 1024},
		Adapter:      appcatalog.Adapter{Name: id, Version: 1},
		Platforms:    []appcatalog.Platform{{OS: "linux", Arch: "amd64"}},
		Dependencies: []appcatalog.PackageRef{}, Provides: []string{"app." + id},
		Conflicts: []string{}, ExclusiveGroups: []string{},
	}, "operator@test"); err != nil {
		t.Fatalf("發佈 manifest %s@%s: %v", id, version, err)
	}
}

func (f *profileFleet) profile(t *testing.T, id string, revision int64, packageID, version string) string {
	t.Helper()
	record, err := f.store.PublishMachineProfile(appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: id, Revision: revision,
		Packages: []appcatalog.PackageRef{{PackageID: packageID, Version: version}},
	}, "operator@test")
	if err != nil {
		t.Fatalf("發佈 profile %s@%d: %v", id, revision, err)
	}
	return record.Digest
}

// assignProfile 寫一筆「這台現在身上是這一版」。
//
// ⚠ 不帶套件列：這一頁讀的「它點名了什麼」來自 profile 自己，不是來自指派那張
// 子表。一份沒有套件列的指派在帳本上是合法的形狀，store 那一層已經釘住了。
func (f *profileFleet) assignProfile(t *testing.T, machineKey, profileID string,
	revision int64, digest string, at time.Time,
) string {
	t.Helper()
	f.nextAssignment++
	assignmentID := "assignment-" + strconv.FormatInt(f.nextAssignment, 10)
	machineID := f.ids[machineKey]
	var previous any
	var current int64
	var previousID string
	if err := f.store.DB().QueryRow(`SELECT assignment_id,assignment_revision
 FROM machine_profile_assignments WHERE machine_id=? ORDER BY assignment_revision DESC LIMIT 1`,
		machineID).Scan(&previousID, &current); err == nil {
		previous = previousID
	}
	if _, err := f.store.DB().Exec(`INSERT INTO machine_profile_assignments
 (assignment_id,machine_id,assignment_revision,profile_id,profile_revision,profile_digest,
  target_os,target_arch,assigned_at,assigned_by,supersedes_assignment_id)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		assignmentID, machineID, current+1, profileID, revision, digest,
		"linux", "amd64", at.UTC().Format(time.RFC3339), "operator@test", previous); err != nil {
		t.Fatalf("寫指派 %s → %s@%d: %v", machineKey, profileID, revision, err)
	}
	return assignmentID
}

// profileFixture 造出正式庫 2026-09-12 真的量到的那個形狀，加上另外三種這一頁必須
// 分得出來的形狀。
//
//	openclaw-standard rev 3  點名 openclaw 2026.9.2 —— 沒有指派過、沒有看到過
//	                         一台都沒指派，而它就是最新的一版 → 發佈了，一台都沒指派
//	openclaw-standard rev 2  點名 openclaw 2026.6.6  —— 沒指派過但 samplehub1／sampleagent3 看得到
//	                         sampleagent2 身上是它                  → 機隊上有機器穿著這一版
//	openclaw-standard rev 1  點名 openclaw 2026.5.20 —— 指派過也看得到
//	                         沒有人穿，而它有更新的 revision    → 已經發佈到更新的版本
//	edge-standard     rev 1  點名 openclaw 2026.5.26 —— 指派過（sampleagent4＋canary）但沒看到
//	                         只有已退役的 old-box 身上是它      → 只有已退役的機器身上還是這一版
//	legacy-standard   rev 1  點名 legacy-tool 0.1.0  —— 指派過（給已退役那台）
//	                         只有已退役的 old-box 回報看得到    → 退役的觀測不算，所以是「沒有一台回報它」
func profileFixture(t *testing.T) *profileFleet {
	t.Helper()
	fleet := &profileFleet{installFleet: installFixture(t)}
	fleet.manifest(t, "openclaw", "2026.5.20", strings.Repeat("1", 64))
	fleet.manifest(t, "openclaw", "2026.5.26", strings.Repeat("2", 64))
	fleet.manifest(t, "openclaw", "2026.6.6", strings.Repeat("3", 64))
	fleet.manifest(t, "openclaw", "2026.9.2", strings.Repeat("4", 64))
	fleet.manifest(t, "legacy-tool", "0.1.0", strings.Repeat("5", 64))
	// ⚠ resource_kind ≠ resource_id 的形狀。openclaw 那幾筆兩欄剛好一樣，所以只有
	// 這一筆問得出「指派過哪一版」是拿 resource_id 對 package_id、不是拿 kind。
	fleet.manifest(t, "hermes-1.4.2", "1.4.2", strings.Repeat("6", 64))
	fleet.assign(t, "machine", fleet.ids["sampleagent3"], "hermes-agent", "hermes-1.4.2",
		`{"kind":"hermes-agent","version":"1.4.2"}`, fleet.now.Add(-20*time.Hour))

	at := fleet.now.Add(-12 * time.Hour)
	fleet.profile(t, "openclaw-standard", 1, "openclaw", "2026.5.20")
	inUse := fleet.profile(t, "openclaw-standard", 2, "openclaw", "2026.6.6")
	fleet.profile(t, "openclaw-standard", 3, "openclaw", "2026.9.2")
	edge := fleet.profile(t, "edge-standard", 1, "openclaw", "2026.5.26")
	fleet.profile(t, "legacy-standard", 1, "legacy-tool", "0.1.0")
	fleet.profile(t, "hermes-standard", 1, "hermes-1.4.2", "1.4.2")

	fleet.assignProfile(t, "sampleagent2", "openclaw-standard", 2, inUse, at)
	fleet.assignProfile(t, "retired", "edge-standard", 1, edge, at)
	return fleet
}

func profileReportOf(t *testing.T, fleet *profileFleet) ProfileReport {
	t.Helper()
	report, err := New(fleet.store).ProfileReport(fleet.now)
	if err != nil {
		t.Fatalf("發佈的 vs 指派的: %v", err)
	}
	return report
}

func profileRowNamed(t *testing.T, report ProfileReport, id string, revision int64) ProfileRow {
	t.Helper()
	for _, row := range report.Profiles {
		if row.ProfileID == id && row.Revision == revision {
			return row
		}
	}
	t.Fatalf("報告裡沒有 %s rev %d：%+v", id, revision, report.Profiles)
	return ProfileRow{}
}

func profilePackageOf(t *testing.T, row ProfileRow, packageID string) ProfilePackage {
	t.Helper()
	for _, pkg := range row.Packages {
		if pkg.PackageID == packageID {
			return pkg
		}
	}
	t.Fatalf("%s rev %d 沒有點名 %s：%+v", row.ProfileID, row.Revision, packageID, row.Packages)
	return ProfilePackage{}
}

// 分母必須跟註冊報告與每機安裝狀態是同一個數。三頁各自算一次的話，同一台機器會在
// 三個畫面上有三個答案。
func TestTheProfileReportCountsTheSameDenominatorAsTheOtherReports(t *testing.T) {
	fleet := profileFixture(t)
	service := New(fleet.store)
	report := profileReportOf(t, fleet)
	enrollment, err := service.EnrollmentReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Machines != enrollment.Denominator || report.Machines != 5 {
		t.Errorf("分母對不上：profile %d、註冊報告 %d（應該是 5）",
			report.Machines, enrollment.Denominator)
	}
	if report.Wearing+report.Bare != report.Machines {
		t.Errorf("%d 台身上有 profile ＋ %d 台沒有 ≠ 分母 %d",
			report.Wearing, report.Bare, report.Machines)
	}
	if report.Wearing != 1 || report.Bare != 4 {
		t.Errorf("穿著 profile 的台數=%d、沒有的=%d，機隊上只有 sampleagent2 身上有",
			report.Wearing, report.Bare)
	}
}

// ⚠⚠ 這是這一頁存在的理由。一份「發佈了、最新的一版、一台都沒指派」的 profile，跟
// 一份「被新版取代了所以沒有人穿」的舊 revision，在畫面上長得一模一樣——而前者是一
// 個發現，後者是歷史版本的預期樣子。併成一個「沒有指派」的話，唯一值得看的那一列會
// 被埋在一堆歷史版本裡。
func TestAProfilePublishedToNobodyIsNotTheSameAsAnOldRevision(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	newest := profileRowNamed(t, report, "openclaw-standard", 3)
	if newest.State != ProfileUnassigned {
		t.Errorf("最新的一版一台都沒指派，狀態卻是 %q", newest.State)
	}
	old := profileRowNamed(t, report, "openclaw-standard", 1)
	if old.State != ProfileReplaced {
		t.Errorf("被新版取代的舊 revision，狀態卻是 %q", old.State)
	}
	if newest.State == old.State {
		t.Error("兩種形狀被寫成同一個狀態")
	}
	if newest.NextStep == "" {
		t.Error("一台都沒指派的那一版沒有下一步")
	}
	if old.NextStep != "" {
		t.Errorf("被取代的舊 revision 被交代了一件事去做：%q", old.NextStep)
	}
}

// 只有已退役的機器身上還是這一版，跟誰都沒穿是兩件事：前者要決定那一版還留不留，
// 後者要決定要不要指派出去。
func TestAProfileWornOnlyByARetiredMachineIsItsOwnState(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	row := profileRowNamed(t, report, "edge-standard", 1)
	if row.State != ProfileRetiredOnly {
		t.Fatalf("只有退役機器穿著它，狀態卻是 %q", row.State)
	}
	if row.AssignedOn != 0 || row.RetiredOn != 1 {
		t.Errorf("機隊上 %d 台、已退役 %d 台，應該是 0 與 1", row.AssignedOn, row.RetiredOn)
	}
	if len(row.Machines) != 1 || !row.Machines[0].Retired {
		t.Fatalf("那一台沒有被標成已退役：%+v", row.Machines)
	}
	if row.Machines[0].DisplayName != "old-box" {
		t.Errorf("穿著它的是 %q", row.Machines[0].DisplayName)
	}
}

// 正在管著機器的那一版要講得出是哪幾台、哪一筆指派、誰指派的——否則畫面上那個數字
// 查不回帳本。
func TestAProfileInUseNamesTheMachinesWearingItAndTheLedgerRow(t *testing.T) {
	fleet := profileFixture(t)
	report := profileReportOf(t, fleet)
	row := profileRowNamed(t, report, "openclaw-standard", 2)
	if row.State != ProfileInUse {
		t.Fatalf("有機器穿著它，狀態卻是 %q", row.State)
	}
	if row.AssignedOn != 1 || len(row.Machines) != 1 {
		t.Fatalf("穿著它的台數=%d、列出 %d 台", row.AssignedOn, len(row.Machines))
	}
	machine := row.Machines[0]
	if machine.MachineID != fleet.ids["sampleagent2"] || machine.DisplayName != "sampleagent2" {
		t.Errorf("穿著它的是 %s／%s", machine.MachineID, machine.DisplayName)
	}
	if machine.AssignmentID == "" || machine.AssignmentRevision != 1 {
		t.Errorf("查不回帳本那一列：assignment=%q revision=%d",
			machine.AssignmentID, machine.AssignmentRevision)
	}
	if machine.AssignedBy != "operator@test" || machine.AssignedAt.IsZero() {
		t.Errorf("誰在什麼時候指派的沒有帶出來：by=%q at=%s",
			machine.AssignedBy, machine.AssignedAt)
	}
	if row.NextStep != "" {
		t.Errorf("正在管著機器的那一版被交代了一件事去做：%q", row.NextStep)
	}
}

// ⚠⚠ 正式庫上那一份 profile 點名 openclaw 2026.9.2，而這個 Hub 從來沒有指派過
// 這一版，四台上也沒有一台是它。那是一句非常具體的話，而且只能由 Hub 自己持有的
// 兩件事說出來——它不等於「2026.9.2 不存在」，這個 Hub 沒有資格講那句話。
func TestAVersionThisHubNeitherAssignedNorSawIsItsOwnState(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	row := profileRowNamed(t, report, "openclaw-standard", 3)
	pkg := profilePackageOf(t, row, "openclaw")
	if pkg.Version != "2026.9.2" {
		t.Fatalf("點名的版本是 %q", pkg.Version)
	}
	if pkg.State != ProfilePackageNeither {
		t.Errorf("沒指派過也沒看到過的版本，狀態卻是 %q", pkg.State)
	}
	if pkg.Intents != 0 || pkg.SeenOn != 0 || pkg.LastAssignedAt != nil {
		t.Errorf("憑空生出了指派或觀測：intents=%d seen=%d last=%v",
			pkg.Intents, pkg.SeenOn, pkg.LastAssignedAt)
	}
	if !strings.Contains(row.Headline, "沒有指派過") {
		t.Errorf("那一列的一句話沒有講出這件事：%q", row.Headline)
	}
}

// 「指派過但沒看到」與「看得到但沒指派過」是兩個相反的發現：前者是工作單那一側的
// 事，後者是有人從指派以外的路徑裝上去的。合成一個「有沒有」會把它們寫成同一格。
func TestSeenButNeverAssignedIsNotTheSameAsAssignedButNeverSeen(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	seen := profilePackageOf(t, profileRowNamed(t, report, "openclaw-standard", 2), "openclaw")
	if seen.State != ProfilePackageSeenNotAssigned {
		t.Errorf("samplehub1 與 sampleagent3 身上就是 2026.6.6，而沒有指派過它，狀態卻是 %q", seen.State)
	}
	if seen.SeenOn != 2 || seen.Intents != 0 {
		t.Errorf("2026.6.6：看到 %d 台、指派 %d 次，應該是 2 與 0", seen.SeenOn, seen.Intents)
	}
	assigned := profilePackageOf(t, profileRowNamed(t, report, "edge-standard", 1), "openclaw")
	if assigned.State != ProfilePackageAssignedNotSeen {
		t.Errorf("2026.5.26 指派過（sampleagent4＋canary）而沒有一台回報它，狀態卻是 %q", assigned.State)
	}
	if assigned.Intents != 2 || assigned.SeenOn != 0 {
		t.Errorf("2026.5.26：指派 %d 次、看到 %d 台，應該是 2 與 0",
			assigned.Intents, assigned.SeenOn)
	}
	if assigned.LastAssignedAt == nil {
		t.Error("指派過卻講不出最後一次是什麼時候")
	}
}

// 兩邊都成立的那一版要兩個數字都帶著：少了任何一個，畫面上就只能說「有」。
func TestAVersionAssignedAndSeenCarriesBothSides(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	pkg := profilePackageOf(t, profileRowNamed(t, report, "openclaw-standard", 1), "openclaw")
	if pkg.State != ProfilePackageAssignedAndSeen {
		t.Fatalf("2026.5.20 指派過也看得到，狀態卻是 %q", pkg.State)
	}
	if pkg.Intents != 1 || pkg.SeenOn != 1 {
		t.Errorf("2026.5.20：指派 %d 次、看到 %d 台，應該是 1 與 1", pkg.Intents, pkg.SeenOn)
	}
	if pkg.NextStep != "" {
		t.Errorf("兩邊都成立的那一格被交代了一件事去做：%q", pkg.NextStep)
	}
}

// ⚠ 退役機器的觀測不算「機隊上看得到」。old-box 身上就是 legacy-tool 0.1.0，而它
// 已經離開分母——把它算進來的話，一個機隊上其實已經沒有的版本會顯示成還在用。
func TestARetiredMachinesObservationNeverCountsAsSeenOnTheFleet(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	pkg := profilePackageOf(t, profileRowNamed(t, report, "legacy-standard", 1), "legacy-tool")
	if pkg.SeenOn != 0 {
		t.Errorf("退役機器的觀測被算進機隊：seen=%d", pkg.SeenOn)
	}
	if pkg.State != ProfilePackageAssignedNotSeen {
		t.Errorf("指派過（給已退役那台）而機隊上看不到，狀態卻是 %q", pkg.State)
	}
}

// 四種 profile 狀態與四種套件狀態全部要在這個機隊上出現，而且每一種都要有它自己的
// 句子。一個沒有人做得出來的狀態，是一個沒有人驗證過的分支。
func TestEveryProfileStateIsReachableAndCarriesItsOwnSentences(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	profiles := map[ProfileState]int{}
	for _, count := range report.States {
		profiles[count.State] = count.Count
		if count.Title == "" || count.Meaning == "" {
			t.Errorf("%q 少了句子：title=%q meaning=%q", count.State, count.Title, count.Meaning)
		}
	}
	for _, stateValue := range ProfileStates() {
		if profiles[stateValue] == 0 {
			t.Errorf("%q 在這個機隊上一列都沒有", stateValue)
		}
	}
	packages := map[ProfilePackageState]int{}
	for _, count := range report.PackageStates {
		packages[count.State] = count.Count
		if count.Title == "" || count.Meaning == "" {
			t.Errorf("%q 少了句子：title=%q meaning=%q", count.State, count.Title, count.Meaning)
		}
	}
	for _, stateValue := range ProfilePackageStates() {
		if packages[stateValue] == 0 {
			t.Errorf("%q 在這個機隊上一格都沒有", stateValue)
		}
	}
}

// 畫面上的數字要在畫面上加得起來。加不起來的時候，讀的人會以為是自己數錯了。
func TestTheProfileCountsAddUpOnScreen(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	if report.Published != len(report.Profiles) {
		t.Errorf("說發佈了 %d 版，列出 %d 版", report.Published, len(report.Profiles))
	}
	total := 0
	for _, count := range report.States {
		total += count.Count
	}
	if total != report.Published {
		t.Errorf("狀態加總 %d ≠ 發佈了 %d 版", total, report.Published)
	}
	cells, states := 0, 0
	for _, row := range report.Profiles {
		cells += len(row.Packages)
		if row.AssignedOn+row.RetiredOn != len(row.Machines) {
			t.Errorf("%s rev %d：%d ＋ %d ≠ 列出的 %d 台",
				row.ProfileID, row.Revision, row.AssignedOn, row.RetiredOn, len(row.Machines))
		}
	}
	for _, count := range report.PackageStates {
		states += count.Count
	}
	if states != cells {
		t.Errorf("套件狀態加總 %d ≠ 實際 %d 格", states, cells)
	}
}

// ⚠⚠ 一份沒有人穿的 profile 不是失敗、不是廢棄、也不是過期。它可能就是備著的。
// 這一頁講兩個事實與它們的方向，沒有一句是判決。
func TestTheProfileReportNeverCallsAnythingAFailure(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	banned := []string{"失敗", "廢棄", "過期", "落後", "孤兒", "沒用", "錯誤", "違規"}
	var sentences []string
	sentences = append(sentences, report.Headline, report.Caveat, report.NextStep)
	for _, count := range report.States {
		sentences = append(sentences, count.Title, count.Meaning, count.NextStep)
	}
	for _, count := range report.PackageStates {
		sentences = append(sentences, count.Title, count.Meaning, count.NextStep)
	}
	for _, row := range report.Profiles {
		sentences = append(sentences, row.Title, row.Meaning, row.NextStep, row.Headline)
		for _, pkg := range row.Packages {
			sentences = append(sentences, pkg.Title, pkg.Meaning, pkg.NextStep)
		}
	}
	for _, sentence := range sentences {
		for _, word := range banned {
			if strings.Contains(sentence, word) {
				t.Errorf("這一頁講了一句判決：%q 裡有 %q", sentence, word)
			}
		}
	}
}

// ⚠ 這個 Hub 沒有上游版本來源，所以它不准講「最新」。那句限制必須跟報告一起走。
func TestTheProfileReportCarriesTheLimitItCanSpeakTo(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	if report.Caveat != ProfileReportCaveat {
		t.Fatalf("那句限制不在報告上：%q", report.Caveat)
	}
	for _, sentence := range []string{report.Headline, report.NextStep} {
		if strings.Contains(sentence, "最新版") || strings.Contains(sentence, "上游") {
			t.Errorf("這一頁講了一句它沒有來源的話：%q", sentence)
		}
	}
}

// 還沒有發佈任何 profile 的 Hub 回的是一句話，不是一份空報告。空的畫面分不出
// 「還沒有人發佈」與「這一頁壞了」。
func TestAHubWithNoProfilePublishedSaysSoInOneSentence(t *testing.T) {
	fleet := installFixture(t)
	report, err := New(fleet.store).ProfileReport(fleet.now)
	if err != nil {
		t.Fatalf("空目錄回錯誤：%v", err)
	}
	if report.Published != 0 || len(report.Profiles) != 0 {
		t.Fatalf("憑空生出了 profile：%+v", report.Profiles)
	}
	if report.Headline != "還沒有發佈任何 profile。" {
		t.Errorf("開場白=%q", report.Headline)
	}
	if report.NextStep == "" {
		t.Error("沒有發佈過任何 profile，卻沒有交代下一步")
	}
	if report.Machines != 5 {
		t.Errorf("分母 %d，名冊上有 5 台", report.Machines)
	}
}

// ⚠⚠ 「這個 Hub 指派過這一版沒有」是拿 profile 點名的 package_id 去對意圖的
// resource_id，不是對 resource_kind。openclaw 那幾筆兩欄剛好一樣，所以拿錯欄位
// 的話整個機隊都看不出來——而 hermes 這一筆的 kind 是 hermes-agent、id 是
// hermes-1.4.2，拿 kind 去對就會說「這個 Hub 沒有指派過 hermes-1.4.2 的 1.4.2」。
func TestWhetherAVersionWasAssignedIsKeyedOnTheResourceNotItsKind(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	pkg := profilePackageOf(t, profileRowNamed(t, report, "hermes-standard", 1), "hermes-1.4.2")
	if pkg.State != ProfilePackageAssignedNotSeen {
		t.Fatalf("hermes-1.4.2@1.4.2 指派過而沒有一台回報它，狀態卻是 %q", pkg.State)
	}
	if pkg.Intents != 1 {
		t.Errorf("指派次數=%d，帳本上有 1 列", pkg.Intents)
	}
}

// ⚠ 「哪一版是最新的」不准靠讀取器回來的順序。MachineProfiles 現在是照 revision
// 由大到小排的，所以「第一個看到的」剛好就是最大的——那是一個會在查詢改了之後
// 安靜壞掉的巧合，而壞掉的樣子是一份舊 revision 被講成「最新的一版卻一台都沒指派」。
func TestTheNewestRevisionIsTheLargestNotTheFirstOneRead(t *testing.T) {
	ascending := []store.MachineProfileRecord{
		{Profile: appcatalog.MachineProfile{ID: "openclaw-standard", Revision: 1}},
		{Profile: appcatalog.MachineProfile{ID: "openclaw-standard", Revision: 2}},
		{Profile: appcatalog.MachineProfile{ID: "openclaw-standard", Revision: 3}},
	}
	if got := profileNewestRevisions(ascending)["openclaw-standard"]; got != 3 {
		t.Errorf("由小到大讀進來時說最新的是 rev %d", got)
	}
	descending := []store.MachineProfileRecord{ascending[2], ascending[1], ascending[0]}
	if got := profileNewestRevisions(descending)["openclaw-standard"]; got != 3 {
		t.Errorf("由大到小讀進來時說最新的是 rev %d", got)
	}
}

// ⚠ 同一個資源在兩種 resource_kind 底下被指派過同一版：兩邊都算數。收攏的鍵是
// （資源, 版本），而 profile 點名的只有 package_id 與版號——它沒有 kind 可以拿來
// 分辨，所以這一層就得把兩邊加起來。少了這一步，畫面上會說「指派過 1 次」而帳本上
// 有 2 列，那個數字對不回去。
func TestOneResourceAssignedUnderTwoKindsCountsBothIntents(t *testing.T) {
	early := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	late := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	got := profileAssignedVersions([]store.InstallIntentVersion{
		{ResourceKind: "openclaw", ResourceID: "openclaw", Version: "2026.6.6",
			Intents: 2, FirstAt: late, LastAt: late},
		{ResourceKind: "openclaw-canary", ResourceID: "openclaw", Version: "2026.6.6",
			Intents: 1, FirstAt: early, LastAt: early},
	})
	row, ok := got[profileVersionKey{packageID: "openclaw", version: "2026.6.6"}]
	if !ok {
		t.Fatalf("那一版不見了：%+v", got)
	}
	if row.Intents != 3 {
		t.Errorf("指派次數=%d，兩種 kind 加起來是 3", row.Intents)
	}
	if !row.FirstAt.Equal(early) || !row.LastAt.Equal(late) {
		t.Errorf("兩端沒有取到兩邊的極值：first=%s last=%s",
			row.FirstAt.Format(time.RFC3339), row.LastAt.Format(time.RFC3339))
	}
}

// 順序每次都一樣。一份會被拿去比對的清單如果每次順序不同，讀的人分不出「這一列是
// 新的」與「它只是換了位置」。
func TestProfilesComeBackInOneFixedOrder(t *testing.T) {
	fleet := profileFixture(t)
	want := []string{
		"edge-standard@1", "hermes-standard@1", "legacy-standard@1",
		"openclaw-standard@3", "openclaw-standard@2", "openclaw-standard@1",
	}
	for round := 0; round < 3; round++ {
		report := profileReportOf(t, fleet)
		if len(report.Profiles) != len(want) {
			t.Fatalf("第 %d 次回了 %d 列", round+1, len(report.Profiles))
		}
		for index, row := range report.Profiles {
			got := row.ProfileID + "@" + strconv.FormatInt(row.Revision, 10)
			if got != want[index] {
				t.Fatalf("第 %d 次的第 %d 列是 %s，應該是 %s", round+1, index+1, got, want[index])
			}
		}
	}
}

// ⚠ 匯出檔裡那一格擠著穿這一版的機器，而已退役的那幾台必須自己標出來。少了那個
// 標記，一份「只有已退役的機器身上還是這一版」的 profile 在試算表裡看起來還在用，
// 而它其實一台在分母裡的機器都沒有。
func TestTheProfileCSVMarksARetiredWearerAsRetired(t *testing.T) {
	fleet := profileFixture(t)
	document := ProfileReportCSV(profileReportOf(t, fleet))
	var found bool
	for _, row := range document.Rows {
		if row[0] != "edge-standard" {
			continue
		}
		found = true
		if row[5] != "old-box（已退役）" {
			t.Errorf("穿著它的那一格是 %q，沒有標出已退役", row[5])
		}
		if row[3] != "0" || row[4] != "1" {
			t.Errorf("機隊上 %q 台、已退役 %q 台，應該是 0 與 1", row[3], row[4])
		}
	}
	if !found {
		t.Fatalf("匯出裡沒有 edge-standard 那一列：%v", document.Rows)
	}
}

// ⚠⚠ 一份什麼都沒點名的 profile 照樣有一列。少了那一列，它在匯出檔裡完全不存在，
// 而 0 列跟「這份 profile 不存在」在試算表裡長得一模一樣。
//
// ⚠ 這裡直接組一份報告，不走目錄：目錄現在擋著零個套件的 profile（見下一支測試），
// 所以這條路今天到不了這個分支。它不是猜的分支——它是往安全方向走的那一支，而且
// 就在這裡被驗過：目錄哪天放寬，這一列也不會安靜消失。
func TestAProfileThatNamesNothingStillHasItsOwnRow(t *testing.T) {
	row := profileRowFor(store.MachineProfileRecord{
		Profile:     appcatalog.MachineProfile{ID: "empty-standard", Revision: 1},
		Digest:      strings.Repeat("a", 64),
		PublishedAt: time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC),
		PublishedBy: "operator@test",
	}, nil, nil, nil, map[string]int64{"empty-standard": 1})
	if row.State != ProfileUnassigned {
		t.Fatalf("狀態=%q", row.State)
	}
	if !strings.Contains(row.Headline, "一個套件都沒點名") {
		t.Errorf("那一列的一句話沒有講出這件事：%q", row.Headline)
	}
	document := ProfileReportCSV(ProfileReport{Profiles: []ProfileRow{row}})
	if len(document.Rows) != 1 {
		t.Fatalf("匯出了 %d 列：%v", len(document.Rows), document.Rows)
	}
	csvRow := document.Rows[0]
	if len(csvRow) != len(document.Columns) {
		t.Fatalf("那一列 %d 格，欄位有 %d 個", len(csvRow), len(document.Columns))
	}
	if csvRow[0] != "empty-standard" {
		t.Errorf("那一列講的是 %q", csvRow[0])
	}
	for _, cell := range csvRow[10:] {
		if cell != "" {
			t.Errorf("什麼都沒點名的那一列帶著套件欄位：%q", cell)
		}
	}
}

// 目錄今天擋著零個套件的 profile。釘住它，是為了讓上面那支測試的「這條路到不了」
// 有憑據——而不是讓下一個人以為那個分支是別人隨手加的。
func TestTheCatalogRefusesAProfileThatNamesNothing(t *testing.T) {
	fleet := profileFixture(t)
	if _, err := fleet.store.PublishMachineProfile(appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "empty-standard", Revision: 1,
		Packages: []appcatalog.PackageRef{},
	}, "operator@test"); err == nil {
		t.Fatal("目錄收下了一份什麼都沒點名的 profile")
	}
}

// 匯出的每一列格數都要跟欄位定義一樣，否則 ReportCSV 會整份拒收——而發現的時候
// 是有人點了「匯出」拿到一個錯誤，不是這裡。
func TestEveryProfileCSVRowHasExactlyTheDeclaredCells(t *testing.T) {
	document := ProfileReportCSV(profileReportOf(t, profileFixture(t)))
	if len(document.Rows) == 0 {
		t.Fatal("fixture 一列都沒有，這支測試沒有在測東西")
	}
	for index, row := range document.Rows {
		if len(row) != len(document.Columns) {
			t.Fatalf("第 %d 列 %d 格，欄位有 %d 個", index+1, len(row), len(document.Columns))
		}
	}
	if _, err := ReportCSV(document); err != nil {
		t.Fatalf("這份匯出寫不出來：%v", err)
	}
}

// 沒有時刻就沒有報告：一份算不出「什麼時候算的」的報告，讀的人沒有辦法知道它多舊。
func TestAProfileReportWithoutATimeIsRefused(t *testing.T) {
	fleet := profileFixture(t)
	if _, err := New(fleet.store).ProfileReport(time.Time{}); !errors.Is(err, ErrInvalidProfileReport) {
		t.Fatalf("沒有時刻的請求被接受了：%v", err)
	}
}

// ⚠⚠ 這一頁不必處理「名冊上沒有這台」，因為帳本擋著那件事：指派那張表的
// machine_id 有一條指到 machine_registry 的外鍵。這支測試釘住的就是那個理由——
// 少了它，下一個人會在投影裡補一支跳過的分支，而那支分支會默默把「身上有 profile
// 的台數」算少，並且永遠不會有人驗證過。
func TestTheLedgerRefusesAnAssignmentForAMachineThatIsNotOnTheRoster(t *testing.T) {
	fleet := profileFixture(t)
	_, err := fleet.store.DB().Exec(`INSERT INTO machine_profile_assignments
 (assignment_id,machine_id,assignment_revision,profile_id,profile_revision,profile_digest,
  target_os,target_arch,assigned_at,assigned_by,supersedes_assignment_id)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"assignment-ghost", "machine-that-is-gone", 1, "openclaw-standard", 3,
		profileDigestOf(t, fleet, "openclaw-standard", 3),
		"linux", "amd64", fleet.now.Add(-time.Hour).UTC().Format(time.RFC3339),
		"operator@test", nil)
	if err == nil {
		t.Fatal("帳本收下了一筆名冊上沒有那台機器的指派")
	}
	report := profileReportOf(t, fleet)
	if report.Wearing != 1 {
		t.Errorf("身上有 profile 的台數=%d，機隊上只有 sampleagent2", report.Wearing)
	}
}

// 退役只是在名冊那一列蓋一個時間，不會把列拿掉——所以一台退役機器身上的指派仍然
// 查得到它是誰。這是「只有已退役的機器身上還是這一版」那個狀態站得住的前提。
func TestRetiringAMachineKeepsItsAssignmentReadable(t *testing.T) {
	fleet := profileFixture(t)
	row := profileRowNamed(t, profileReportOf(t, fleet), "edge-standard", 1)
	if len(row.Machines) != 1 {
		t.Fatalf("退役機器身上的指派不見了：%+v", row.Machines)
	}
	if row.Machines[0].MachineID != fleet.ids["retired"] {
		t.Errorf("穿著它的是 %s，應該是已退役那台", row.Machines[0].MachineID)
	}
}

func profileDigestOf(t *testing.T, fleet *profileFleet, id string, revision int64) string {
	t.Helper()
	record, err := fleet.store.MachineProfile(id, revision)
	if err != nil {
		t.Fatalf("讀 profile %s@%d: %v", id, revision, err)
	}
	return record.Digest
}

// ⚠⚠ 「看得到」照樣算「看得到」。samplehub1 身上真的有 2026.6.6 那個檔案——站不住的是
// 「所以它正在跑那一版」。把它從 SeenOn 裡拿掉，一份其實裝上去了的 profile 會變成
// 「沒有一台回報它」，然後有人會再指派一次。
func TestAVersionMeasuredOnAFileNobodyRunsStillCountsAsSeen(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	pkg := profilePackageOf(t, profileRowNamed(t, report, "openclaw-standard", 2), "openclaw")
	if pkg.SeenOn != 2 {
		t.Fatalf("samplehub1 與 sampleagent3 都回報 2026.6.6，看到的台數卻是 %d", pkg.SeenOn)
	}
	if pkg.SeenMisattributedOn != 1 {
		t.Errorf("samplehub1 那個版號量的是沒在跑的那一份，卻算成 %d 台", pkg.SeenMisattributedOn)
	}
	// 它還是「看得到」那一軸上的同一種狀態——量錯檔案不是第五種狀態。
	if _, everSeen, _ := ProfilePackageStateAxes(pkg.State); !everSeen {
		t.Errorf("量錯檔案把「看得到」那一軸關掉了：狀態是 %q", pkg.State)
	}
}

// ⚠⚠ 兩邊都成立、而看到的那一台量的是一個已經從磁碟上不見的檔案——這是這一頁上最讓
// 人放下心的一格。它沒有下一步（那是這個狀態的意思），所以那個數字是畫面上唯一講得出
// 「這一格的證據有多硬」的東西。
func TestTheMostReassuringProfileCellCanRestEntirelyOnAFileNobodyRuns(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	row := profileRowNamed(t, report, "openclaw-standard", 1)
	pkg := profilePackageOf(t, row, "openclaw")
	if pkg.State != ProfilePackageAssignedAndSeen || pkg.NextStep != "" {
		t.Fatalf("2026.5.20 該是指派過也看得到、而且沒有下一步：state=%q next=%q",
			pkg.State, pkg.NextStep)
	}
	if pkg.SeenOn != 1 || pkg.SeenMisattributedOn != 1 {
		t.Fatalf("sampleagent2 在跑的那個檔案已經不見了：seen=%d misattributed=%d",
			pkg.SeenOn, pkg.SeenMisattributedOn)
	}
	if !strings.Contains(row.Headline, "另有 1 個看得到的版號量的是沒在跑的那一份") {
		t.Errorf("那一列的一句話少了這件事：%s", row.Headline)
	}
}

// 在跑的就是量版號的那一個檔案，那一台不算。把「量的就是跑的」算進去，機隊上每一格都
// 會變成待辦事項，然後沒有人再看這個數字。
func TestTheMachineWhoseRunningFileIsTheMeasuredOneIsNotCounted(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	pkg := profilePackageOf(t, profileRowNamed(t, report, "openclaw-standard", 2), "openclaw")
	if pkg.SeenOn-pkg.SeenMisattributedOn != 1 {
		t.Errorf("sampleagent3 在跑的就是量的那一份，卻沒有留在乾淨的那一邊：seen=%d misattributed=%d",
			pkg.SeenOn, pkg.SeenMisattributedOn)
	}
	if pkg.SeenMisattributedOn > pkg.SeenOn {
		t.Errorf("量錯檔案的台數比看到的台數還多：%d > %d", pkg.SeenMisattributedOn, pkg.SeenOn)
	}
}

// ⚠ 退役機器的觀測不算「看得到」，所以也不算「量錯了檔案」。fixture 裡已退役那台的
// legacy-tool 量的就不是它在跑的那一份——退役那一關要排在這一問前面，反過來的話，
// 一台已經離開機隊的機器會在這一頁上留下一筆待辦。
func TestARetiredMachineNeverContributesAMisattributedSeenCell(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	pkg := profilePackageOf(t, profileRowNamed(t, report, "legacy-standard", 1), "legacy-tool")
	if pkg.SeenOn != 0 || pkg.SeenMisattributedOn != 0 {
		t.Errorf("退役機器留下了一格：seen=%d misattributed=%d", pkg.SeenOn, pkg.SeenMisattributedOn)
	}
	if report.SeenMisattributed != 2 {
		t.Errorf("整份報告數到 %d 格，退役那一台被算進去了", report.SeenMisattributed)
	}
}

// 整份報告那個數字算的是「一份 revision × 一個套件版本」那個格子，跟匯出檔一列同一個
// 粒度；改成算台數的話，同一台機器會在好幾份 profile 上被重複算進來。
func TestTheProfileReportCountsTheCellsWhoseSeenEvidenceIsNotRunning(t *testing.T) {
	report := profileReportOf(t, profileFixture(t))
	cells := 0
	for _, row := range report.Profiles {
		for _, pkg := range row.Packages {
			if pkg.SeenMisattributedOn > 0 {
				cells++
			}
		}
	}
	if cells != 2 {
		t.Fatalf("逐格數出 %d 格（2026.5.20 與 2026.6.6），這支測試認得的是 2", cells)
	}
	if report.SeenMisattributed != cells {
		t.Errorf("報告說 %d 格，逐格數出 %d 格", report.SeenMisattributed, cells)
	}
	if !strings.Contains(report.Headline, "另有 2 格看得到的版號量的是沒在跑的那一份") {
		t.Errorf("整份報告那一行字少了這一句：%s", report.Headline)
	}
}

// ⚠⚠ 一格裡有兩台量錯仍是一格，不是兩格。只有每一格都剛好一台的 fixture，會讓
// `SeenMisattributed++` 與 `SeenMisattributed += SeenMisattributedOn` 看起來是一樣的算法。
func TestTwoMachinesMeasuringTheWrongFileStillMakeOneProfileCell(t *testing.T) {
	fleet := profileFixture(t)
	if err := fleet.store.RecordObservation(fleet.ids["sampleagent3"], model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: fleet.now.Add(-30 * time.Second),
		CLITools: []model.CLITool{{
			Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "2026.6.6", Path: installSystemCopy, RealPath: installSystemCopy,
			RunningPID: 3310, RunningScript: installReleaseCopy,
		}},
	}, fleet.now); err != nil {
		t.Fatal(err)
	}
	report := profileReportOf(t, fleet)
	pkg := profilePackageOf(t, profileRowNamed(t, report, "openclaw-standard", 2), "openclaw")
	if pkg.SeenMisattributedOn != 2 {
		t.Fatalf("同一格應該有兩台量錯，數出 %d 台", pkg.SeenMisattributedOn)
	}
	if report.SeenMisattributed != 2 {
		t.Errorf("整份報告應該仍是兩格，不是三台：%d", report.SeenMisattributed)
	}
}

// 下一步的順序：先講「這個 Hub 沒有指派過也沒有看到過」（那一版哪裡都不存在），再講
// 「看得到的版號量的是沒在跑的那一份」（證據不硬），最後才講「一台都沒指派」。
//
// ⚠ 量錯檔案排在「一台都沒指派」前面：一份看起來已經上去了的 profile 可能只是在硬碟
// 上放著，先確定看到的是哪一份檔案，再談哪一份還沒指派。
func TestTheProfileNextStepAsksTheHarderQuestionsFirst(t *testing.T) {
	withCounts := func(neither, misattributed, unassigned int) ProfileReport {
		report := ProfileReport{
			Published: 1, SeenMisattributed: misattributed, Unassigned: unassigned,
		}
		report.PackageStates = append(report.PackageStates, ProfilePackageStateCount{
			State: ProfilePackageNeither, Count: neither,
		})
		return report
	}
	for name, shape := range map[string]struct {
		report ProfileReport
		want   string
	}{
		"三件事都有":    {withCounts(1, 1, 1), "先看那幾個這個 Hub 沒有指派過也沒有看到過的套件版本。"},
		"量錯檔案與沒指派": {withCounts(0, 1, 1), "先看那幾格看得到的版號量的是沒在跑的那一份，再談哪一份還沒指派。"},
		"只有沒指派":    {withCounts(0, 0, 1), "先看那幾份發佈了卻一台都沒指派的 profile。"},
		"都沒有":      {withCounts(0, 0, 0), ""},
	} {
		if got := profileReportNextStep(shape.report); got != shape.want {
			t.Errorf("%s：下一步是 %q，應該是 %q", name, got, shape.want)
		}
	}
}

// ⚠⚠ 加一個欄位就要把版號 +1。對面的 strict decoder 對著一個它不認得的欄位會整份拒
// 收，所以悄悄加欄位會讓舊的用戶端在解碼那一刻才壞。
func TestAFieldAddedToTheProfileReportComesWithANewSchemaVersion(t *testing.T) {
	if ProfileReportSchemaVersion != 2 {
		t.Fatalf("版號是 %d；欄位清單跟它必須一起動", ProfileReportSchemaVersion)
	}
	for _, shape := range []struct {
		name   string
		fields []string
		value  any
	}{
		{"ProfileReport", []string{
			"schema_version", "evaluated_at", "machines", "wearing", "bare",
			"published", "unassigned", "seen_misattributed",
			"profiles", "states", "package_states", "headline", "caveat", "next_step",
		}, ProfileReport{}},
		{"ProfilePackage", []string{
			"package_id", "version", "state", "title", "meaning", "next_step",
			"intents", "last_assigned_at", "seen_on", "seen_misattributed_on",
		}, ProfilePackage{}},
	} {
		got := softwareJSONFields(shape.value)
		if strings.Join(got, ",") != strings.Join(shape.fields, ",") {
			t.Errorf("%s 的欄位是 %v，這支測試認得的是 %v", shape.name, got, shape.fields)
		}
	}
}

// 匯出的檔案要答得出「這一格的看得到有多硬」。有人把它開在試算表裡照「機隊上看到幾台」
// 排序的時候，那一欄是唯一看得出來的東西。
func TestTheProfileCSVSaysHowManyOfTheSeenMachinesMeasuredADeadFile(t *testing.T) {
	document := ProfileReportCSV(profileReportOf(t, profileFixture(t)))
	index := map[string]int{}
	for i, column := range document.Columns {
		index[column.Key] = i
	}
	if _, ok := index["seen_misattributed_on"]; !ok {
		t.Fatalf("CSV 少了那一欄：%+v", document.Columns)
	}
	var found bool
	for _, row := range document.Rows {
		if row[index["profile"]] != "openclaw-standard" || row[index["revision"]] != "2" {
			continue
		}
		found = true
		if row[index["seen_on"]] != "2" || row[index["seen_misattributed_on"]] != "1" {
			t.Errorf("openclaw-standard rev 2 那一列：seen=%q misattributed=%q",
				row[index["seen_on"]], row[index["seen_misattributed_on"]])
		}
	}
	if !found {
		t.Fatalf("CSV 裡沒有 openclaw-standard rev 2 那一列")
	}
}
