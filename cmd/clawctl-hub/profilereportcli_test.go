package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

// profileCLIFixture 造出這份報告四種狀態裡最難講清楚的三種，加上兩個軸的三種組合：
//
//	openclaw-standard rev 2  點名 openclaw 2026.9.2 —— 指派過沒有、看到過沒有都是「沒有」，
//	                         而且一台都沒指派、它就是最新的一版（正式庫 2026-09-12 的形狀）
//	openclaw-standard rev 1  點名 openclaw 2026.5.20 —— 指派過也看得到；cnode-operator 穿著它，
//	                         而那個版號量在一份沒有人在跑的安裝上
//	legacy-standard rev 1    點名 legacy-tool 0.1.0  —— 只有已退役的那台身上還是它
func profileCLIFixture(t *testing.T) (string, machineCommandDeps) {
	t.Helper()
	f, now := reportFixture(t)
	gone, _, err := f.store.CreateEnrollTokenFor("old-box", 0)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠⚠ 版號量在 login copy 上，而在跑的是 releases/<ver>/ 那一份（正式庫 samplehub1 從
	// 2026-09-06 起就是這個形狀）。少了這一格，「指派過這一版，機隊上也看得到」在
	// terminal 上看起來永遠是硬證據。
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		CLITools: []model.CLITool{{Name: "openclaw", Present: true, OnPath: true,
			PresentEvidence: "path", VersionReported: "2026.5.20",
			Path: "/usr/local/bin/openclaw", RealPath: "/usr/local/bin/openclaw",
			RunningPID:    4131,
			RunningScript: "/home/example-user/.local/share/clawctl/releases/2026.6.6/openclaw"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	// ⚠ 直接寫帳本。受管目錄的 spec 走不了 CreateDesiredState（它要求先有一筆 profile
	// 指派或部署），而正式庫裡 openclaw 的那幾列就是從部署那條路進來的——這份報告讀的
	// 是那幾列的歷史，所以 fixture 要造出的正是同一個形狀。
	tx, err := f.store.DB().Begin()
	if err != nil {
		t.Fatalf("開始寫意圖：%v", err)
	}
	defer tx.Rollback()
	var desiredRevision int64
	if err := tx.QueryRow(`INSERT INTO revision_counters (resource_scope,current_revision)
 VALUES ('openclaw:openclaw',1)
 ON CONFLICT(resource_scope) DO UPDATE
 SET current_revision=revision_counters.current_revision+1
 RETURNING current_revision`).Scan(&desiredRevision); err != nil {
		t.Fatalf("配意圖版號：%v", err)
	}
	if _, err := tx.Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES(?,'machine',?,'openclaw','openclaw',?,?,?,'operator@test')`,
		"desired-2026.5.20", f.machine.id, desiredRevision,
		`{"kind":"openclaw","version":"2026.5.20"}`,
		now.Add(-24*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("寫意圖：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交意圖：%v", err)
	}
	for _, manifest := range []struct{ id, version, sha string }{
		{"openclaw", "2026.5.20", strings.Repeat("a", 64)},
		{"openclaw", "2026.9.2", strings.Repeat("b", 64)},
		{"legacy-tool", "0.1.0", strings.Repeat("c", 64)},
	} {
		if _, err := f.store.PublishCatalogManifest(appcatalog.Manifest{
			SchemaVersion: appcatalog.SchemaVersion, ID: manifest.id, Version: manifest.version,
			Kind: appcatalog.KindApp, Title: manifest.id,
			Source: appcatalog.Source{
				Catalog: "ai-intune", UpstreamURL: "https://example.invalid/" + manifest.id,
				Revision: manifest.version, License: "MIT",
			},
			Artifact:     appcatalog.Artifact{SHA256: manifest.sha, Size: 1024},
			Adapter:      appcatalog.Adapter{Name: manifest.id, Version: 1},
			Platforms:    []appcatalog.Platform{{OS: "linux", Arch: "amd64"}},
			Dependencies: []appcatalog.PackageRef{}, Provides: []string{"app." + manifest.id},
			Conflicts: []string{}, ExclusiveGroups: []string{},
		}, "operator@test"); err != nil {
			t.Fatalf("發佈 manifest %s %s：%v", manifest.id, manifest.version, err)
		}
	}
	digests := map[string]string{}
	for _, profile := range []struct {
		id        string
		revision  int64
		packageID string
		version   string
	}{
		{"openclaw-standard", 1, "openclaw", "2026.5.20"},
		{"openclaw-standard", 2, "openclaw", "2026.9.2"},
		{"legacy-standard", 1, "legacy-tool", "0.1.0"},
	} {
		record, err := f.store.PublishMachineProfile(appcatalog.MachineProfile{
			SchemaVersion: appcatalog.SchemaVersion, ID: profile.id, Revision: profile.revision,
			Packages: []appcatalog.PackageRef{{PackageID: profile.packageID, Version: profile.version}},
		}, "operator@test")
		if err != nil {
			t.Fatalf("發佈 profile %s rev %d：%v", profile.id, profile.revision, err)
		}
		// ⚠ digest 要連 revision 一起記。帳本的 trigger 會核對指派帶的 digest 就是
		// 那一版發佈的那一個——只用 profile id 當鍵，rev 1 的指派會帶著 rev 2 的 digest。
		digests[fmt.Sprintf("%s/%d", profile.id, profile.revision)] = record.Digest
	}
	for _, assignment := range []struct {
		id, machineID, profileID string
		revision                 int64
	}{
		{"assignment-worn", f.machine.id, "openclaw-standard", 1},
		{"assignment-gone", gone, "legacy-standard", 1},
	} {
		if _, err := f.store.DB().Exec(`INSERT INTO machine_profile_assignments
 (assignment_id,machine_id,assignment_revision,profile_id,profile_revision,profile_digest,
  target_os,target_arch,assigned_at,assigned_by,supersedes_assignment_id)
 VALUES (?,?,1,?,?,?,'linux','amd64',?,'operator@test',NULL)`,
			assignment.id, assignment.machineID, assignment.profileID, assignment.revision,
			digests[fmt.Sprintf("%s/%d", assignment.profileID, assignment.revision)],
			now.Add(-12*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatalf("寫指派 %s：%v", assignment.id, err)
		}
	}
	// ⚠ 退役是在指派之後才發生的，順序反過來也一樣：退役只蓋一個時刻，指派那一列
	// 照樣留在帳本上。這份報告要講得出「穿著它的機器都退役了」跟「誰都沒穿」的差別。
	// CreateEnrollTokenFor 走 Store 時鐘，不是 fixture 凍結的 Hub 時鐘；跨秒時，凍結的 now
	// 可能比 created_at 早一秒，所以這個 lifecycle event 要從當下的有效時刻衍生。
	retiredAt := time.Now().UTC()
	if err := f.store.RetireMachine(gone, retiredAt); err != nil {
		t.Fatal(err)
	}
	return reportCLIServer(t, f)
}

func runProfileCLI(t *testing.T, argv ...string) string {
	t.Helper()
	base, deps := profileCLIFixture(t)
	var out, errOut bytes.Buffer
	if err := runProfileReportCommandWithDeps(t.Context(),
		append([]string{"--hub-url", base}, argv...), &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	return out.String()
}

// CLI 上的那一份要跟畫面回答同一句話：這份 profile 發佈了，然後呢。
//
// ⚠⚠ 包含那句限制。terminal 上少了它，一格「這個 Hub 沒有指派過這一版，也沒有看到
// 過」會被讀成「這一版不存在」——而這個 Hub 沒有上游版本來源，它沒有資格講那句話。
func TestProfileCLIPrintsWhatWasPublishedAndWhatWasAssigned(t *testing.T) {
	text := runProfileCLI(t)
	for _, want := range []string{
		operator.ProfileReportCaveat,
		"published profile revisions", "revision", "in fleet", "retired", "published at", "next step",
		"package", "version", "assignments", "last assigned", "seen on",
		"openclaw-standard", "legacy-standard", "2026.5.20", "2026.9.2", "0.1.0",
		"old-box", "retired",
		operator.ProfileStateTitle(operator.ProfileInUse),
		operator.ProfileStateTitle(operator.ProfileRetiredOnly),
		operator.ProfileStateTitle(operator.ProfileUnassigned),
		operator.ProfileStateNextStep(operator.ProfileUnassigned),
		operator.ProfilePackageStateTitle(operator.ProfilePackageAssignedAndSeen),
		operator.ProfilePackageStateTitle(operator.ProfilePackageNeither),
		operator.ProfilePackageStateNextStep(operator.ProfilePackageNeither),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
}

// ⚠⚠ 一份發佈了卻一台都沒指派的 profile 不是失敗，也不是設定錯誤——它可能本來就只是
// 備著。把它講成失敗，操作員會去「修」一個沒有壞的東西。
func TestProfileCLINeverCallsAPublishedProfileAFailure(t *testing.T) {
	text := runProfileCLI(t)
	for _, banned := range []string{"失敗", "錯誤", "設定錯", "落後", "不合規", "違規"} {
		if strings.Contains(text, banned) {
			t.Errorf("輸出出現 %q：\n%s", banned, text)
		}
	}
}

// ⚠ 退役的那台身上還是 legacy-standard rev 1，而那一版的「機隊上」必須是 0。少了
// 這個分別，一份沒有任何在籍機器在用的 profile 在 terminal 上看起來還在用。
func TestProfileCLIKeepsARetiredWearerOutOfTheFleetCount(t *testing.T) {
	text := runProfileCLI(t)
	var found bool
	for _, line := range strings.Split(text, "\n") {
		// 表上那一列是 profile／revision／這一版是什麼／機隊上／已退役／發佈的時刻／下一步。
		// ⚠ 只認 revision 那一欄是數字的那一行——每一版自己那一段的開頭也是
		// 「legacy-standard rev 1：…」，而那一句話裡的台數不是表上那兩欄。
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "legacy-standard" || fields[1] != "1" {
			continue
		}
		found = true
		if want := operator.ProfileStateTitle(operator.ProfileRetiredOnly); fields[2] != want {
			t.Errorf("legacy-standard rev 1 那一列是 %q，應該是 %q", fields[2], want)
		}
		if fields[3] != "0" || fields[4] != "1" {
			t.Errorf("legacy-standard rev 1 的台數是機隊上 %s、已退役 %s，應該是 0 與 1",
				fields[3], fields[4])
		}
	}
	if !found {
		t.Fatalf("輸出沒有 legacy-standard 那一列：\n%s", text)
	}
	if !strings.Contains(text, "old-box") || !strings.Contains(text, "retired") {
		t.Fatalf("輸出沒有講出那台退役的機器是誰：\n%s", text)
	}
}

func TestProfileCLIEmitsTheSameDocumentAsTheAPI(t *testing.T) {
	var report operator.ProfileReport
	body := runProfileCLI(t, "--json")
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v\n%s", err, body)
	}
	if report.SchemaVersion != operator.ProfileReportSchemaVersion ||
		report.Caveat != operator.ProfileReportCaveat ||
		report.Published != len(report.Profiles) || len(report.Profiles) != 3 {
		t.Fatalf("report=%+v", report)
	}
	if report.Wearing+report.Bare != report.Machines {
		t.Fatalf("%d+%d 台，分母是 %d 台", report.Wearing, report.Bare, report.Machines)
	}
	if report.Unassigned != 1 {
		t.Fatalf("發佈了一台都沒指派的有 %d 版，fixture 造了 1 版", report.Unassigned)
	}
	if len(report.States) != len(operator.ProfileStates()) ||
		len(report.PackageStates) != len(operator.ProfilePackageStates()) {
		t.Fatalf("狀態有 %d／%d 種，這個版本認得 %d／%d 種",
			len(report.States), len(report.PackageStates),
			len(operator.ProfileStates()), len(operator.ProfilePackageStates()))
	}
}

// ⚠⚠ 匯出的一列是「一版 × 它點名的一個套件版本」。用機器當列的話，一份一台都沒指派
// 的 profile 在檔案裡一列都沒有——而 0 列跟「這份 profile 不存在」在試算表裡一樣。
func TestProfileCLIExportsOneRowPerRevisionPerPackage(t *testing.T) {
	rows := reportCLIExportRows(t, runProfileCLI(t, "--csv"))
	var report operator.ProfileReport
	if err := json.Unmarshal([]byte(runProfileCLI(t, "--json")), &report); err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, row := range report.Profiles {
		want += max(len(row.Packages), 1)
	}
	if len(rows) != want+1 {
		t.Fatalf("匯出 %d 列（含表頭），報告有 %d 列", len(rows), want)
	}
	columns := operator.ReportCSVColumns(operator.ProfileReportCSV(report))
	if len(rows[0]) != len(columns) {
		t.Fatalf("表頭 %d 欄，文件說 %d 欄", len(rows[0]), len(columns))
	}
	for index, column := range columns {
		if rows[0][index] != column.Header {
			t.Errorf("第 %d 欄是 %q，文件說 %q", index, rows[0][index], column.Header)
		}
	}
	// ⚠ 那句限制是最後一欄，而且每一列都帶著：一列離開這個檔案之後，讀它的人還是
	// 要看得到這個 Hub 不知道什麼。
	for index, row := range rows[1:] {
		if row[len(row)-1] != operator.ProfileReportCaveat {
			t.Fatalf("第 %d 列沒有帶那句限制：%v", index+1, row)
		}
	}
}

// ⚠ --json 與 --csv 是兩種完整的輸出，同時給的時候要拒絕，不是挑一個。默默挑一個的
// 話，腳本會拿到一份它沒有要的格式，而它只會發現「解不開」。
func TestProfileCLIRefusesTwoOutputFormatsAtOnce(t *testing.T) {
	base, deps := profileCLIFixture(t)
	var out, errOut bytes.Buffer
	err := runProfileReportCommandWithDeps(t.Context(),
		[]string{"--hub-url", base, "--json", "--csv"}, &out, &errOut, deps)
	if err == nil {
		t.Fatalf("--json --csv 同時給沒有被拒絕：%s", out.String())
	}
	if !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("err=%v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("被拒絕了還是印了東西：%s", out.String())
	}
}

func TestProfileCLIRefusesPositionalArguments(t *testing.T) {
	base, deps := profileCLIFixture(t)
	var out, errOut bytes.Buffer
	err := runProfileReportCommandWithDeps(t.Context(),
		[]string{"--hub-url", base, "openclaw-standard"}, &out, &errOut, deps)
	if err == nil {
		t.Fatalf("positional argument 沒有被拒絕：%s", out.String())
	}
	// ⚠ 默默忽略一個 profile 名字，呼叫端會以為它拿到的是篩過的那一份，而它拿到的
	// 是全部。
	if !strings.Contains(err.Error(), "positional arguments") {
		t.Fatalf("err=%v", err)
	}
}

// ⚠⚠ 那幾格排在已發佈那張表前面。terminal 上一個只看前面兩張表的人，會把
// 「指派過這一版，機隊上也看得到」讀成收工——而那一格的證據量在一份沒有人在跑的安裝上。
func TestProfileCLILeadsWithTheSeenEvidenceThatIsNotRunning(t *testing.T) {
	text := runProfileCLI(t)
	head := strings.Index(text, "1 visible versions measure an installation that is not running")
	table := strings.Index(text, "published profile revisions")
	if head < 0 {
		t.Fatalf("輸出沒有那一段：\n%s", text)
	}
	if table < head {
		t.Errorf("已發佈那張表排在那一段前面：%d < %d\n%s", table, head, text)
	}
	section := text[head:table]
	if !strings.Contains(section, "1 visible versions measure an installation that is not running") {
		t.Errorf("那一段沒有數出 1 格：\n%s", section)
	}
	for _, want := range []string{
		"seen on", "measured wrong", "openclaw-standard", "2026.5.20",
		operator.ProfilePackageStateTitle(operator.ProfilePackageAssignedAndSeen),
		"To see which file those machines measured",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("那一段少了 %q：\n%s", want, section)
		}
	}
	// ⚠ 這一段不印那一格自己的下一步。「指派過這一版，機隊上也看得到」的下一句是空的，
	// 印成「—」會在這一段裡讀成沒事要做。
	if strings.Contains(section, "—") {
		t.Errorf("那一段印了一個看起來像「沒事」的格子：\n%s", section)
	}
	// 那一版一個套件都沒被指派過也沒看到過的那一格不可以出現在這一段裡。
	if strings.Contains(section, "2026.9.2") {
		t.Errorf("沒有人看到過的那一版混進了這一段：\n%s", section)
	}
	var found bool
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[0] != "openclaw-standard" || fields[2] != "openclaw" {
			continue
		}
		found = true
		if fields[4] != "1" || fields[5] != "1" {
			t.Errorf("那一格的看到台數與其中量錯是 %q／%q，應該都是 1：%s",
				fields[4], fields[5], line)
		}
	}
	if !found {
		t.Errorf("那一段沒有 openclaw-standard 的那一格：\n%s", section)
	}
}

// 逐版那一段的「看到幾台」自己要帶著這件事。直接捲到某一份 profile 的人只看那一張表。
func TestProfileCLISaysHowManyOfTheSeenMachinesMeasuredADeadFile(t *testing.T) {
	text := runProfileCLI(t)
	detail := text[strings.Index(text, "package states"):]
	var found bool
	for _, line := range strings.Split(detail, "\n") {
		fields := strings.Fields(line)
		// 逐版那張表一列是 套件／版號／這一版是什麼／指派過幾次／最後一次指派／看到幾台／其中量錯／下一步。
		if len(fields) < 7 || fields[0] != "openclaw" || fields[1] != "2026.5.20" {
			continue
		}
		found = true
		if fields[len(fields)-2] != "1" || fields[len(fields)-3] != "1" {
			t.Errorf("那一列的看到台數與其中量錯是 %q／%q，應該都是 1\n%s",
				fields[len(fields)-3], fields[len(fields)-2], line)
		}
	}
	if !found {
		t.Fatalf("逐版那一段沒有 openclaw 2026.5.20 那一列：\n%s", detail)
	}
	if !strings.Contains(text, "measured wrong") {
		t.Errorf("逐版那張表沒有那一欄：\n%s", text)
	}
}

// 整份報告那一行字與那一句下一步都要講得出這件事。捲到最後只看那兩句的人，靠的就是它們。
func TestProfileCLIHeadlineCountsTheSeenEvidenceThatIsNotRunning(t *testing.T) {
	var report operator.ProfileReport
	if err := json.Unmarshal([]byte(runProfileCLI(t, "--json")), &report); err != nil {
		t.Fatal(err)
	}
	if report.SeenMisattributed != 1 {
		t.Fatalf("fixture 數出 %d 格，這支測試認得的是 1", report.SeenMisattributed)
	}
	text := runProfileCLI(t)
	if !strings.Contains(text, "另有 1 格看得到的版號量的是沒在跑的那一份") {
		t.Errorf("那一行字少了這一句：\n%s", text)
	}
	// ⚠ fixture 裡有一格「這個 Hub 沒有指派過也沒有看到過」，而那一問排在更前面——
	// 那一版哪裡都不存在，比「證據不硬」更早要答。整份報告那一句下一步要原樣印在最後。
	if report.NextStep != "先看那幾個這個 Hub 沒有指派過也沒有看到過的套件版本。" {
		t.Fatalf("下一步是 %q", report.NextStep)
	}
	if !strings.HasSuffix(strings.TrimSpace(text), report.NextStep) {
		t.Errorf("那一句下一步沒有印在最後：\n%s", text)
	}
}
