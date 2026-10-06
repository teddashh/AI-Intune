package web

import (
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// profilePageFleet 造出這一頁真正要講的三種形狀：
//
//	openclaw-standard rev 3  點名 openclaw 2026.9.2 —— 這個 Hub 沒有指派過也沒看到過，
//	                         而且一台都沒指派、它就是最新的一版（正式庫 2026-09-12 的形狀）
//	openclaw-standard rev 2  點名 openclaw 2026.6.6  —— samplehub1 身上就是它，而沒有指派過
//	                         sampleagent2 穿著這一版；samplehub1 量的就是它在跑的那一份
//	openclaw-standard rev 1  點名 openclaw 2026.5.20 —— 指派過也看得到；沒有人穿而有更新的版本
//	                         而 sampleagent2 那個版號量在一份沒有人在跑的安裝上
func profilePageFleet(t *testing.T, st *store.Store) (wearing string) {
	t.Helper()
	now := time.Now().UTC()
	wearing = enroll(t, st, "sampleagent2", now.Add(-time.Hour))
	seen := enroll(t, st, "samplehub1", now.Add(-time.Hour))
	enroll(t, st, "sampleagent4", now.Add(-time.Hour))

	observe := func(id string, tool model.CLITool) {
		t.Helper()
		tool.Name, tool.Present, tool.OnPath, tool.PresentEvidence = "openclaw", true, true, "path"
		if err := st.RecordObservation(id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-2 * time.Minute),
			CLITools: []model.CLITool{tool},
		}, now.Add(-time.Minute)); err != nil {
			t.Fatalf("記觀測: %v", err)
		}
	}
	// ⚠⚠ sampleagent2 那一格是「指派過這一版，機隊上也看得到」——這一頁上最讓人放下心的一格
	// ——而那個版號量在 login copy 上，機隊從 2026-09-06 起改從 releases/<ver>/ 跑（正式庫
	// samplehub1 就是這個形狀）。少了這一格，這一頁的「看得到」看起來永遠是硬證據。
	observe(wearing, model.CLITool{VersionReported: "2026.5.20",
		Path: "/usr/local/bin/openclaw", RealPath: "/usr/local/bin/openclaw",
		RunningPID:    4131,
		RunningScript: "/home/example-user/.local/share/clawctl/releases/2026.6.6/openclaw"})
	// samplehub1 量的就是它在跑的那一份：這一格必須留在乾淨的那一邊。
	observe(seen, model.CLITool{VersionReported: "2026.6.6",
		Path: "/usr/local/bin/openclaw", RunningPID: 3310,
		RunningExe: "/usr/local/bin/openclaw"})

	if _, err := st.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES(?,'machine',?,'openclaw','openclaw',1,?,?,'operator@test')`,
		"desired-2026.5.20", wearing, `{"kind":"openclaw","version":"2026.5.20"}`,
		now.Add(-24*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("寫意圖: %v", err)
	}

	for _, version := range []string{"2026.5.20", "2026.6.6", "2026.9.2"} {
		if _, err := st.PublishCatalogManifest(appcatalog.Manifest{
			SchemaVersion: appcatalog.SchemaVersion, ID: "openclaw", Version: version,
			Kind: appcatalog.KindApp, Title: "OpenClaw",
			Source: appcatalog.Source{
				Catalog: "ai-intune", UpstreamURL: "https://example.invalid/openclaw",
				Revision: version, License: "MIT",
			},
			Artifact:     appcatalog.Artifact{SHA256: strings.Repeat(version[5:6], 64), Size: 1024},
			Adapter:      appcatalog.Adapter{Name: "openclaw", Version: 1},
			Platforms:    []appcatalog.Platform{{OS: "linux", Arch: "amd64"}},
			Dependencies: []appcatalog.PackageRef{}, Provides: []string{"app.openclaw"},
			Conflicts: []string{}, ExclusiveGroups: []string{},
		}, "operator@test"); err != nil {
			t.Fatalf("發佈 manifest %s: %v", version, err)
		}
	}
	var worn string
	for revision, version := range map[int64]string{1: "2026.5.20", 2: "2026.6.6", 3: "2026.9.2"} {
		record, err := st.PublishMachineProfile(appcatalog.MachineProfile{
			SchemaVersion: appcatalog.SchemaVersion, ID: "openclaw-standard", Revision: revision,
			Packages: []appcatalog.PackageRef{{PackageID: "openclaw", Version: version}},
		}, "operator@test")
		if err != nil {
			t.Fatalf("發佈 profile rev %d: %v", revision, err)
		}
		if revision == 2 {
			worn = record.Digest
		}
	}
	if _, err := st.DB().Exec(`INSERT INTO machine_profile_assignments
 (assignment_id,machine_id,assignment_revision,profile_id,profile_revision,profile_digest,
  target_os,target_arch,assigned_at,assigned_by,supersedes_assignment_id)
 VALUES ('assignment-1',?,1,'openclaw-standard',2,?,'linux','amd64',?,'operator@test',NULL)`,
		wearing, worn, now.Add(-12*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("寫指派: %v", err)
	}
	return wearing
}

// ⚠⚠ 這一頁存在的理由就是那一列：一份發佈了、點名了一個這個 Hub 哪裡都沒見過的
// 版本、而且一台都沒指派的 profile。它必須自己有一節，不能只躺在最下面那張大表裡。
func TestTheProfilePageLeadsWithTheVersionThisHubHasNeverSeen(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	rec := webRequest(t, s, "/reports/profile")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"點名了一個這個 Hub 沒有見過的版本",
		"2026.9.2",
		operator.ProfilePackageStateTitle(operator.ProfilePackageNeither),
		operator.ProfileStateTitle(operator.ProfileUnassigned),
		operator.ProfileReportCaveat,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("這一頁沒有講 %q", want)
		}
	}
}

// 「發佈了一台都沒指派」跟「被更新的版本取代了」在畫面上必須分得開：前者是這一頁
// 要人去看的那一列，後者是歷史版本本來的樣子。混在一起的話，唯一值得看的那一列會
// 被埋在一堆舊 revision 裡。
func TestTheProfilePageSeparatesPublishedToNobodyFromSuperseded(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	body := webRequest(t, s, "/reports/profile").Body.String()
	section := body[strings.Index(body, `id="unassigned"`):]
	if end := strings.Index(section, `id="states"`); end > 0 {
		section = section[:end]
	}
	if !strings.Contains(section, "rev 3") {
		t.Errorf("「發佈了，一台都沒指派」那一節沒有 rev 3：%s", section)
	}
	for _, unwanted := range []string{"rev 1", "rev 2"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("「發佈了，一台都沒指派」那一節混進了 %s", unwanted)
		}
	}
	if !strings.Contains(body, operator.ProfileStateTitle(operator.ProfileReplaced)) {
		t.Error("畫面上沒有「已經發佈到更新的版本」這個狀態")
	}
}

// ⚠ 一份沒有人穿的 profile 不是失敗、不是廢棄、也不是過期。它可能就是備著的。
func TestTheProfilePageNeverCallsAPublishedProfileAFailure(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	body := webRequest(t, s, "/reports/profile").Body.String()
	for _, banned := range []string{"失敗", "廢棄", "過期", "落後", "孤兒", "沒用", "違規"} {
		if strings.Contains(body, banned) {
			t.Errorf("這一頁講了一句判決：%q", banned)
		}
	}
}

// ⚠⚠ 匯出的一列是「一份已發佈的 revision × 它點名的一個套件版本」。改成一台機器
// 一列的話，一份一台都沒指派的 profile 在檔案裡一列都沒有——而 0 列跟「這份
// profile 不存在」在試算表裡長得一模一樣。
func TestTheProfileCSVExportsOneRowPerRevisionPerPackageOverHTTP(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	rec := webRequest(t, s, "/reports/profile.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("content-type=%q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "profile") {
		t.Errorf("content-disposition=%q", got)
	}
	rows, err := csv.NewReader(strings.NewReader(
		strings.TrimPrefix(rec.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.operator.ProfileReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cells := 0
	for _, row := range report.Profiles {
		cells += len(row.Packages)
	}
	if cells == 0 {
		t.Fatal("fixture 一格都沒有，這支測試沒有在測東西")
	}
	if len(rows) != cells+1 {
		t.Fatalf("CSV %d 列（含表頭），畫面上有 %d 格", len(rows), cells)
	}
	if rows[0][0] != "profile" || rows[0][len(rows[0])-1] != "這份報告的限制" {
		t.Fatalf("表頭=%v", rows[0])
	}
	// ⚠ 欄位照表頭找，不照位置。中間插一欄的時候，按位置寫的斷言會安靜地去檢查隔壁
	// 那一欄——而兩欄剛好都是 0 或都是空的時候，它會通過。
	at := map[string]int{}
	for index, header := range rows[0] {
		at[header] = index
	}
	for index, row := range rows[1:] {
		if row[len(row)-1] != operator.ProfileReportCaveat {
			t.Fatalf("第 %d 列沒有帶那句限制：%v", index+1, row)
		}
	}
	// 匯出的檔案要答得出「哪一版一台都沒指派」——那正是有人把它開在試算表裡要問的。
	var found bool
	for _, row := range rows[1:] {
		if row[at["revision"]] != "3" {
			continue
		}
		found = true
		if row[at["這一版現在是什麼"]] != operator.ProfileStateTitle(operator.ProfileUnassigned) {
			t.Errorf("rev 3 那一列是 %q，該是發佈了一台都沒指派", row[at["這一版現在是什麼"]])
		}
		if row[at["機隊上穿著它的台數"]] != "0" || row[at["已退役還穿著它的台數"]] != "0" ||
			row[at["穿著它的機器"]] != "" {
			t.Errorf("一台都沒指派的那一列帶著機器：%q/%q/%q",
				row[at["機隊上穿著它的台數"]], row[at["已退役還穿著它的台數"]],
				row[at["穿著它的機器"]])
		}
		if row[at["版號"]] != "2026.9.2" ||
			row[at["這一版在這個 Hub 身上是什麼"]] !=
				operator.ProfilePackageStateTitle(operator.ProfilePackageNeither) {
			t.Errorf("rev 3 點名的那一版是 %q／%q",
				row[at["版號"]], row[at["這一版在這個 Hub 身上是什麼"]])
		}
		if row[at["指派過幾次"]] != "0" || row[at["機隊上看到幾台"]] != "0" ||
			row[at["其中版號不是跑的那一份的台數"]] != "0" {
			t.Errorf("沒見過的那一版帶著指派或觀測：intents=%q seen=%q misattributed=%q",
				row[at["指派過幾次"]], row[at["機隊上看到幾台"]],
				row[at["其中版號不是跑的那一份的台數"]])
		}
	}
	if !found {
		t.Errorf("CSV 裡沒有 rev 3 那一列：%v", rows)
	}
	// 匯出的檔案要答得出「這一格的看得到有多硬」：rev 1 那一版只有一台看得到，而那一台
	// 量的是一份沒有人在跑的安裝。
	found = false
	for _, row := range rows[1:] {
		if row[at["revision"]] != "1" {
			continue
		}
		found = true
		if row[at["機隊上看到幾台"]] != "1" || row[at["其中版號不是跑的那一份的台數"]] != "1" {
			t.Errorf("rev 1 那一列：seen=%q misattributed=%q",
				row[at["機隊上看到幾台"]], row[at["其中版號不是跑的那一份的台數"]])
		}
		if !strings.Contains(row[at["這個套件版本的下一步"]], "到每機安裝狀態核對執行檔與其版號") {
			t.Errorf("CSV 的 rev 1 沒有指出查核動作：%q", row[at["這個套件版本的下一步"]])
		}
	}
	if !found {
		t.Errorf("CSV 裡沒有 rev 1 那一列：%v", rows)
	}
}

// 這份報告只講「現在」，沒有範圍好挑。
func TestTheProfilePageRejectsAQueryItCannotHonour(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	for _, target := range []string{
		"/reports/profile?days=7", "/reports/profile.csv?profile=openclaw-standard",
	} {
		if rec := webRequest(t, s, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d，應該拒絕", target, rec.Code)
		}
	}
}

// 報告落地頁列出來的，就是選單上的那幾個，也就是真的點得進去的那幾頁。
func TestTheProfileReportIsReachableFromTheReportsIndex(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	body := webRequest(t, s, "/reports").Body.String()
	if !strings.Contains(body, `href="/reports/profile"`) {
		t.Error("報告落地頁上沒有發佈與指派那一列")
	}
	if !strings.Contains(body, `href="/reports/profile.csv"`) {
		t.Error("報告落地頁上沒有發佈與指派的匯出連結")
	}
}

// ⚠⚠ 那幾格「看得到」量的是一份沒有人在跑的安裝，而它們必須排在「發佈了一台都沒指派」
// 前面。「一台都沒指派」講的是還沒開始；這幾格講的是上面每一個「看得到」有多硬——反過來
// 排的話，操作員會先去指派新的，而那一份其實已經在機器的硬碟上放著了。
func TestTheProfilePagePutsTheSoftEvidenceAheadOfTheUnassignedProfiles(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	body := webRequest(t, s, "/reports/profile").Body.String()
	misattributed := strings.Index(body, `id="misattributed"`)
	unassigned := strings.Index(body, `id="unassigned"`)
	if misattributed < 0 {
		t.Fatal("這一頁沒有那一節")
	}
	if unassigned < misattributed {
		t.Errorf("「發佈了一台都沒指派」排在量錯檔案那一節前面：%d < %d",
			unassigned, misattributed)
	}
	section := body[misattributed:unassigned]
	for _, want := range []string{
		"rev 1", "2026.5.20",
		operator.ProfilePackageStateTitle(operator.ProfilePackageAssignedAndSeen),
	} {
		if !strings.Contains(section, want) {
			t.Errorf("那一節沒有講 %q：%s", want, section)
		}
	}
	// 量的就是在跑的那一份的那一格不可以出現在這一節裡。
	if strings.Contains(section, "2026.6.6") {
		t.Errorf("量的就是跑的那一格混進了這一節：%s", section)
	}
}

// 摘要那幾張卡要講得出這個數字。只在下面那一節出現的話，一個把那一節捲過去的人會
// 以為這一頁上的每一個「看得到」都是硬證據。
func TestTheProfilePageCountsTheSoftEvidenceInItsSummary(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	body := webRequest(t, s, "/reports/profile").Body.String()
	grid := body[strings.Index(body, `class="metric-grid"`):]
	grid = grid[:strings.Index(grid, "</section>")]
	if !strings.Contains(grid, "版號不是跑的那一份") {
		t.Errorf("摘要那幾張卡沒有這一格：%s", grid)
	}
	metric := grid[strings.Index(grid, "版號不是跑的那一份"):]
	metric = metric[:strings.Index(metric, "</article>")]
	if !strings.Contains(metric, `<div class="metric-value">1</div>`) {
		t.Errorf("摘要卡沒有數出 1 格：%s", metric)
	}
	report, err := s.operator.ProfileReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.SeenMisattributed != 1 {
		t.Fatalf("fixture 數出 %d 格，這支測試認得的是 1", report.SeenMisattributed)
	}
	// 那一句下一步要先問這件事，再問哪一份還沒指派。
	if !strings.Contains(body, "先看那幾格看得到的版號量的是沒在跑的那一份") &&
		!strings.Contains(body, "先看那幾個這個 Hub 沒有指派過也沒有看到過的套件版本") {
		t.Errorf("這一頁那一句下一步是 %q", report.NextStep)
	}
}

// 逐版那張表上，那一格的「機隊上看到幾台」要自己帶著這件事。只有上面那一節講的話，
// 一個直接捲到某一份 profile 的人會把那個台數當成硬證據。
func TestEachProfileRowSaysHowManyOfItsSeenMachinesMeasuredADeadFile(t *testing.T) {
	s, st := newServer(t)
	profilePageFleet(t, st)
	body := webRequest(t, s, "/reports/profile").Body.String()
	// ⚠ 逐版那幾節是新的在上面（rev 3、2、1），所以 rev 1 那一節到頁尾為止。
	section := body[strings.Index(body, `id="profile-openclaw-standard-1"`):]
	if !strings.Contains(section, "其中 1 台量的是沒在跑的那一份") {
		t.Errorf("rev 1 那一節的看到台數沒有帶著這件事：%s", section)
	}
	if !strings.Contains(section, "到每機安裝狀態核對執行檔與其版號") {
		t.Errorf("rev 1 那一節沒有指出下一步：%s", section)
	}
	clean := body[strings.Index(body, `id="profile-openclaw-standard-2"`):strings.Index(body, `id="profile-openclaw-standard-1"`)]
	if strings.Contains(clean, "量的是沒在跑的那一份") {
		t.Errorf("rev 2 量的就是跑的那一份，卻帶著那一句：%s", clean)
	}
}
