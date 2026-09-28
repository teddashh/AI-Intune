package web

import (
	"encoding/csv"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestTheDisclosurePageNamesEveryCategoryAndWhatItHolds(t *testing.T) {
	s, _ := newServer(t)
	rec := webRequest(t, s, "/tenant/data")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := html.UnescapeString(rec.Body.String())
	disclosure, err := operator.DataDisclosureFor(store.DefaultRetention(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, category := range disclosure.Categories {
		for _, want := range []string{
			category.Title, category.Holds, category.SourceSentence,
			category.RetentionSentence, category.RetirementSentence, category.FreeTextSentence,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 少了 %q", category.Key, want)
			}
		}
	}
	if !strings.Contains(body, "保留期的數字讀的是這個 Hub 現行的設定") {
		t.Error("這一頁沒有講出那些天數是從哪裡來的")
	}
}

// 保留期調短，揭露面必須跟著短。印預設值的揭露面會承諾一段 Hub 已經刪掉的歷史。
func TestTheDisclosurePageFollowsAShortenedRetention(t *testing.T) {
	s, st := newServer(t)
	policy := store.DefaultRetention()
	policy.Checkins = 9 * 24 * time.Hour
	if err := s.SetRetentionPolicy(policy); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(webRequest(t, s, "/tenant/data").Body.String())
	if !strings.Contains(body, "留 9 天") {
		t.Errorf("揭露面沒有跟著現行保留期走：\n%s", body)
	}
	if strings.Contains(body, "留 14 天") {
		t.Error("揭露面還在印預設保留期")
	}
	// 單機那一頁講的是同一份保留期，而且它還要照著它算出清除界線。
	id := onlineMachine(t, st, "samplehub1")
	machineBody := html.UnescapeString(webRequest(t, s, "/machines/"+id+"/data").Body.String())
	if !strings.Contains(machineBody, "留 9 天") || strings.Contains(machineBody, "留 14 天") {
		t.Errorf("單機揭露面沒有跟著現行保留期走：\n%s", machineBody)
	}
	cutoff := time.Now().UTC().Add(-9 * 24 * time.Hour).Local().Format("2006-01-02")
	if !strings.Contains(machineBody, "現在會清掉 "+cutoff) {
		t.Errorf("清除界線不是從現行保留期算出來的（找 %q）：\n%s", cutoff, machineBody)
	}
}

func TestTheDisclosurePageRefusesAQuery(t *testing.T) {
	s, _ := newServer(t)
	if rec := webRequest(t, s, "/tenant/data?days=7"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestTheMachineDataPageCountsEveryCategory(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	rec := webRequest(t, s, "/machines/"+id+"/data")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := html.UnescapeString(rec.Body.String())
	result, err := s.operator.MachineData(id, store.DefaultRetention(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, measured := range result.Categories {
		if !strings.Contains(body, measured.Category.Title) {
			t.Errorf("畫面少了 %s", measured.Category.Key)
		}
	}
	for _, want := range []string{"samplehub1 的資料", "逐類的留存量", "自由文字", "匯出"} {
		if !strings.Contains(body, want) {
			t.Errorf("畫面少了 %q", want)
		}
	}
	if !strings.Contains(body, "還在報到，會繼續增加") {
		t.Error("沒有講出還會不會有新的一列")
	}
}

// 會被保留期清掉的類別要講出現在的界線；不會被清的類別不可以給一條界線，那會讓
// 人以為那些列也會消失。
func TestOnlyTimedCategoriesShowACutoff(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := html.UnescapeString(webRequest(t, s, "/machines/"+id+"/data").Body.String())
	cutoffs := strings.Count(body, "現在會清掉")
	result, err := s.operator.MachineData(id, store.DefaultRetention(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	timed := 0
	for _, measured := range result.Categories {
		if measured.Category.Retention.Kind == operator.DataRetentionTimed {
			timed++
		}
	}
	if cutoffs != timed {
		t.Fatalf("畫面上有 %d 條清除界線，會被時間清的類別有 %d 類", cutoffs, timed)
	}
	if timed == 0 {
		t.Fatal("沒有任何一類會被時間清，那保留期在管什麼")
	}
}

// 摘要卡上的兩個數字跟下面那兩張表講的是同一件事。數字對不上，操作員會以為畫面
// 上沒有列出來的那幾類不存在。
func TestTheSummaryCountsMatchTheTablesBelowThem(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := html.UnescapeString(webRequest(t, s, "/machines/"+id+"/data").Body.String())
	result, err := s.operator.MachineData(id, store.DefaultRetention(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	timed, freeText := 0, []operator.DataCategory{}
	for _, measured := range result.Categories {
		if measured.Category.Retention.Kind == operator.DataRetentionTimed {
			timed++
		}
		if measured.Category.FreeText != "" {
			freeText = append(freeText, measured.Category)
		}
	}
	if timed == 0 || len(freeText) == 0 {
		t.Fatal("這台機器既沒有會被清的類別也沒有自由文字，那這支測試沒有在測東西")
	}
	for want, label := range map[string]string{
		metricValue(timed):         "會被保留期清掉",
		metricValue(len(freeText)): "含自由文字",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("摘要卡上沒有 %s 的 %s：\n%s", label, want, body)
		}
	}
	// 「其餘」就是其餘：會被清的那幾類不能再被算進留著的那一句裡。
	kept := len(result.Categories) - timed
	if kept <= 0 {
		t.Fatal("每一類都會被清，那「其餘」這句話沒有東西可講")
	}
	if want := "其餘 " + strconv.Itoa(kept) + " 類裡的列留著"; !strings.Contains(body, want) {
		t.Errorf("摘要卡沒有 %q，總共 %d 類、其中 %d 類會被清：\n%s",
			want, len(result.Categories), timed, body)
	}
	if bad := "其餘 " + strconv.Itoa(len(result.Categories)) + " 類裡的列留著"; strings.Contains(body, bad) {
		t.Errorf("摘要卡把總類數當成「其餘」印出來了：%q", bad)
	}
	section := body[strings.Index(body, "id=\"free-text\""):]
	for _, category := range freeText {
		if !strings.Contains(section, category.FreeText) {
			t.Errorf("自由文字那一節少了 %s 的 %q", category.Key, category.FreeText)
		}
	}
	for _, measured := range result.Categories {
		if measured.Category.FreeText != "" {
			continue
		}
		if strings.Contains(section, ">"+measured.Category.Title+"<") {
			t.Errorf("%s 沒有自由文字，卻被列在自由文字那一節", measured.Category.Key)
		}
	}
}

func metricValue(count int) string {
	return "<div class=\"metric-value\">" + strconv.Itoa(count) + "</div>"
}

func TestTheMachineDataCSVExportsExactlyWhatIsOnThePage(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	rec := webRequest(t, s, "/machines/"+id+"/data.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("content-type=%q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, id) {
		t.Errorf("content-disposition=%q", got)
	}
	rows, err := csv.NewReader(strings.NewReader(
		strings.TrimPrefix(rec.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(operator.DataCategoryKeys())+1 {
		t.Fatalf("CSV 有 %d 列（含表頭），類別有 %d 類", len(rows), len(operator.DataCategoryKeys()))
	}
	if rows[0][0] != "類別" || rows[0][len(rows[0])-1] != "留的是什麼" {
		t.Fatalf("表頭=%v", rows[0])
	}
}

func TestTheMachineDataPageRefusesWhatItCannotAnswer(t *testing.T) {
	s, _ := newServer(t)
	for path, want := range map[string]int{
		"/machines/不在名冊上/data":     http.StatusNotFound,
		"/machines/不在名冊上/data.csv": http.StatusNotFound,
	} {
		if rec := webRequest(t, s, path); rec.Code != want {
			t.Errorf("%s status=%d, want %d", path, rec.Code, want)
		}
	}
	s2, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	if rec := webRequest(t, s2, "/machines/"+id+"/data?days=7"); rec.Code != http.StatusBadRequest {
		t.Errorf("帶查詢參數的單機揭露面 status=%d", rec.Code)
	}
}
