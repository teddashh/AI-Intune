package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// CLI 上的揭露面要跟畫面回答同樣的問題：這一類留的是什麼、誰產生的、留多久、
// 退役之後還剩什麼。少了任何一件，操作員就得再開一次瀏覽器才答得出「你手上有
// 我的什麼」。
func TestDataDisclosureCLIPrintsEveryCategoryAndEveryClause(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runDataDisclosureCommandWithDeps(t.Context(), []string{"--hub-url", base},
		&out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	text := out.String()
	disclosure, err := operator.DataDisclosureFor(store.DefaultRetention(), hubNow())
	if err != nil {
		t.Fatal(err)
	}
	for _, category := range disclosure.Categories {
		for _, want := range []string{
			category.Title, category.Holds, category.SourceSentence,
			category.RetentionSentence, category.RetirementSentence,
			category.FreeTextSentence, category.Path,
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s 少了 %q：\n%s", category.Key, want, text)
			}
		}
	}
	if !strings.Contains(text, "data categories across") {
		t.Fatalf("沒有總計那一行：\n%s", text)
	}
}

// Hub 自己的詞彙不該被引號包起來印出去。`terminalSafe` 是給真正沒驗過的上游自由
// 文字用的；揭露面的每一句都已經在用戶端驗過控制字元了。
func TestDataDisclosureCLIPrintsHubVocabularyUnquoted(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runDataDisclosureCommandWithDeps(t.Context(), []string{"--hub-url", base},
		&out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	if strings.Contains(out.String(), `"名冊"`) {
		t.Fatalf("Hub 自己的詞彙被加了引號：\n%s", out.String())
	}
}

// 一段很長的「留的是什麼」不能把整張表撐開。撐開之後就看不出哪一類留多久了。
func TestALongDescriptionDoesNotWidenTheDisclosureColumns(t *testing.T) {
	render := func(holds string) string {
		disclosure, err := operator.DataDisclosureFor(store.DefaultRetention(),
			time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		disclosure.Categories[0].Holds = holds
		var out bytes.Buffer
		if err := writeDataDisclosure(&out, disclosure); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	title := operator.DataCategoryKeys()[0]
	short := dataLineContaining(t, render("一句話。"), string("名冊"))
	long := dataLineContaining(t, render(strings.Repeat("這一類留的東西很多。", 200)), "名冊")
	if short != long {
		t.Fatalf("%s：長說明把類別那一列撐開了：\n%q\n%q", title, short, long)
	}
}

func TestMachineDataCLIPrintsEveryCategoryAndTheCutoff(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runMachineDataSubcommand(t.Context(),
		[]string{"--machine", f.machine.id, "--hub-url", base}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"total rows in the Hub", "still checking in, will continue to increase",
		"CATEGORY", "ROWS", "OLDEST (UTC)", "RETENTION",
		"名冊", "報到", "觀測", "稽核記錄", "currently pruning rows received before",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
}

// 退役與否改變的是「還會不會有新的一列」。兩種狀態印同一句，操作員就看不出這台
// 的列數已經不會再動了。
func TestMachineDataCLISaysWhetherMoreRowsAreStillComing(t *testing.T) {
	now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	disclosure, err := operator.DataDisclosureFor(store.DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	render := func(retired bool) string {
		result := operator.MachineDataResult{
			SchemaVersion: operator.DataDisclosureSchemaVersion, EvaluatedAt: now,
			MachineID: "m1", DisplayName: "samplehub1", Retired: retired,
		}
		if retired {
			at := now.Add(-time.Hour)
			result.RetiredAt = &at
		}
		for _, category := range disclosure.Categories {
			result.Categories = append(result.Categories,
				operator.MachineDataCategory{Category: category})
		}
		var out bytes.Buffer
		if err := writeMachineData(&out, result); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if live := render(false); !strings.Contains(live, "still checking in, will continue to increase") {
		t.Errorf("還在報到的機器：\n%s", live)
	}
	retired := render(true)
	if !strings.Contains(retired, "retired, no new rows will be added") {
		t.Errorf("已退役的機器：\n%s", retired)
	}
	if strings.Contains(retired, "still checking in") {
		t.Errorf("已退役的機器還說自己在報到：\n%s", retired)
	}
}

func TestMachineDataCLIEmitsTheSameDocumentAsTheAPI(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runMachineDataSubcommand(t.Context(),
		[]string{"--machine", f.machine.id, "--hub-url", base, "--json"}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	var fromCLI operator.MachineDataResult
	if err := json.Unmarshal(out.Bytes(), &fromCLI); err != nil {
		t.Fatalf("CLI 的 JSON 不合法：%v", err)
	}
	var fromAPI operator.MachineDataResult
	if err := json.Unmarshal(
		reportGet(t, f, "/v1/operator/machines/"+f.machine.id+"/data").Body.Bytes(),
		&fromAPI); err != nil {
		t.Fatal(err)
	}
	if fromCLI.Rows != fromAPI.Rows || len(fromCLI.Categories) != len(fromAPI.Categories) {
		t.Fatalf("CLI=%d 列／%d 類，API=%d 列／%d 類",
			fromCLI.Rows, len(fromCLI.Categories), fromAPI.Rows, len(fromAPI.Categories))
	}
	var csvOut, csvErr bytes.Buffer
	if err := runMachineDataSubcommand(t.Context(),
		[]string{"--machine", f.machine.id, "--hub-url", base, "--csv"}, &csvOut, &csvErr, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, csvErr.String())
	}
	rows := reportCLIExportRows(t, csvOut.String())
	if len(rows) != len(fromCLI.Categories)+1 {
		t.Fatalf("CSV 有 %d 列（含表頭），畫面上有 %d 類", len(rows), len(fromCLI.Categories))
	}
}

func TestDataCLIsRefuseWhatTheyCannotRun(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	for name, argv := range map[string][]string{
		"沒有指定機器":         {"--hub-url", base},
		"json 與 csv 同時":  {"--machine", f.machine.id, "--hub-url", base, "--json", "--csv"},
		"多餘的 positional": {"--machine", f.machine.id, "--hub-url", base, "extra"},
	} {
		var out, errOut bytes.Buffer
		if err := runMachineDataSubcommand(t.Context(), argv, &out, &errOut, deps); err == nil {
			t.Errorf("%s：應該被擋下來", name)
		} else if name == "沒有指定機器" && err.Error() != "machine data: --machine is required" {
			t.Errorf("沒有指定機器的訊息是 %q", err.Error())
		}
	}
	var out, errOut bytes.Buffer
	if err := runDataDisclosureCommandWithDeps(t.Context(),
		[]string{"--hub-url", base, "extra"}, &out, &errOut, deps); err == nil {
		t.Error("data 不該接受 positional arguments")
	}
}

func dataLineContaining(t *testing.T, text, want string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("輸出裡沒有含 %q 的那一列：\n%s", want, text)
	return ""
}
