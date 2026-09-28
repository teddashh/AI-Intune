package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
	"github.com/teddashh/AI-Intune/internal/web"
)

// Retire 的完成判準是三件事同時成立：
//
//	它離開分母  ·  歷史還在  ·  憑證不能用了
//
// ⚠⚠ 這個測試刻意跨越 store / api / web 三層，走真的 HTTP。
//
// 三件事各自都有自己那一層的測試，而且都是綠的。但這條判準要防的
// 失效模式**只存在於它們之間**：只要有任何一個查詢忘了
// `retired_at IS NULL`，退役的機器就會從那一個畫面上回來 ——
// 而那時候三個單層測試依然全綠。docs/PHASE1.md §5.0 的那句話：
// **bug 住在零件之間，所以測試也要跨零件。**
//
// 實測時 Overview 的過濾寫在 Go 裡（`if m.RetiredAt != nil { continue }`），
// 而 AuthenticateAgent 的寫在 SQL 裡。兩個不同的機制答同一個問題，
// 那正是需要一個共同的測試把它們釘在一起的情況。
func TestRetireLeavesDenominatorKeepsHistoryAndKillsTheToken(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	h := &hub{store: st}
	ui, err := web.New(st, "")
	if err != nil {
		t.Fatalf("web: %v", err)
	}
	mux := http.NewServeMux()
	h.machineAndPublicRoutes(mux)
	ui.Routes(mux)

	// --- 兩台機器報到，其中一台等一下要退役。
	keep := enrollViaHTTP(t, mux, st, "keeper")
	gone := enrollViaHTTP(t, mux, st, "leaver")

	// leaver 送一次觀測 —— 這是等一下要確認「還查得到」的那份歷史。
	postObservation(t, mux, gone.token, http.StatusNoContent)

	// --- 退役之前：兩台都在分母裡。
	if body := getPage(t, mux, "/"); !strings.Contains(body, "名冊上 2 台") {
		t.Fatalf("前提錯了，退役前分母應該是 2：\n%s", firstLines(body, 6))
	}

	// --- 退役。
	if err := st.RetireMachine(gone.id, time.Now().UTC()); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// --- 一：離開分母 —— 但**不是**離開畫面。
	//
	// ⚠ 這兩件事原本在這裡是同一條斷言（`!Contains(body, "leaver")`），
	// 而那條斷言強制執行的正好是這個產品要修的那個 bug。見下面那支測試
	// 的註解。所以現在分開：不在機隊表格裡（離開分母），
	// 但在已退役那一段裡（沒有離開畫面）。
	body := getPage(t, mux, "/")
	if !strings.Contains(body, "名冊上 1 台") {
		t.Errorf("退役後分母沒有變成 1：\n%s", firstLines(body, 6))
	}
	if strings.Contains(fleetTable(t, body), "leaver") {
		t.Error("退役的機器還在機隊表格上 —— 那它就沒有離開分母")
	}
	if !strings.Contains(body, "leaver") {
		t.Error("退役的機器從畫面上整個消失了 —— 離開分母不等於離開畫面")
	}
	if !strings.Contains(fleetTable(t, body), "keeper") {
		t.Error("退役一台把另一台也弄不見了")
	}

	// --- 二：歷史還在。詳細頁要照樣打得開，而且看得到那次觀測。
	//
	// ⚠ 這一條是 retire 與 delete 唯一的差別。「這台退場之前發生了什麼」
	// 是事後唯一查得到的線索，刪掉它等於把退役變成銷毀證據。
	detail := getPage(t, mux, "/machines/"+gone.id)
	if !strings.Contains(detail, "leaver") {
		t.Error("退役機器的詳細頁打不開了 —— 歷史沒有留住")
	}
	if !strings.Contains(detail, "2026.6.1") {
		t.Errorf("看不到退役前那次觀測的內容：\n%s", firstLines(detail, 4))
	}

	// --- 三：憑證不能用了。
	//
	// ⚠ 判準上寫的是 403，實際回的是 401。這裡**不改程式去迎合文件**：
	// authed() 刻意不透露 token 是「不存在」還是「不對」，而 401 是
	// 「這張憑證我不認」的正確語意。判準要的是「進不來」，那成立。
	// 文件上那個 403 是簡寫，不是規格。
	postObservation(t, mux, gone.token, http.StatusUnauthorized)
	postCheckin(t, mux, gone.token, http.StatusUnauthorized)

	// --- 沒退役的那台不准被連累。
	postObservation(t, mux, keep.token, http.StatusNoContent)
}

// TestRetiredMachineLeavesTheDenominatorWithoutLeavingTheScreen
//
// ⚠ 這個測試存在的理由：分母的規則在四個地方各寫了一次 ——
// Overview 在 Go 裡、ticket 週報在 SQL 裡、死人之鐘又是另一句 SQL、
// tailnet 對照是第四個。四個各自會忘記的地方。
//
// ⚠⚠ 這個測試原本的名字是 `...DoesNotComeBackThroughAnyPage`，
// 而它斷言的是 `!strings.Contains(body, "leaver")` ——
// **它強制執行了「機器從畫面上消失」，而那正好是這個產品存在要修的那個 bug。**
//
// 它綠了好幾天。實機退役 sampleagent1 兩秒才看到真相：機隊表格上沒有它，
// 而唯一還提到它的一列在「名冊之外」那一段，寫著「不在名冊裡」、綠燈
// online、附一句「不打算納管就跑 ignore-peer」。三句話全錯。
// 同時「取消退役」那個按鈕變成一扇沒有門的房間 —— retire 在 UI 上
// 不可逆了，而「可以反悔」是那個寫入路徑自己立的規則。
//
// **離開分母**跟**離開畫面**是兩件事。測試只寫了前者的名字，
// 卻斷言了後者的行為 —— 所以現在兩件都斷言，而且分開斷言。
func TestRetiredMachineLeavesTheDenominatorWithoutLeavingTheScreen(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	h := &hub{store: st}
	ui, err := web.New(st, "")
	if err != nil {
		t.Fatalf("web: %v", err)
	}
	mux := http.NewServeMux()
	h.machineAndPublicRoutes(mux)
	ui.Routes(mux)

	enrollViaHTTP(t, mux, st, "keeper")
	gone := enrollViaHTTP(t, mux, st, "leaver")
	postObservation(t, mux, gone.token, http.StatusNoContent)
	if err := st.RetireMachine(gone.id, time.Now().UTC()); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// ⚠⚠ 釘住 tailnet 的答案，而且刻意讓 leaver **在** tailnet 上而且 online。
	//
	// 不釘的話這一段會去 exec 真的 tailscale，於是「名冊之外」列的是跑測試
	// 那台機器上真實的鄰居，而 leaver 從來不是其中之一 —— 下面第 (3) 條
	// 斷言就永遠不可能失敗。那不是覆蓋，那是一句空話。
	//
	// 而「退役了、但那台機器其實還活著」正好是這裡最需要測的狀態：
	// 它是人手動宣告的「不管了」跟第三方觀測到的「還在跑」之間的矛盾。
	ui.SetTailnetStatus(tailnet.Status{
		Available: true,
		Self:      tailnet.Peer{Hostname: "hub", IP: "100.64.0.1", OS: "linux", Online: true},
		Peers: []tailnet.Peer{
			{Hostname: "keeper", IP: "100.64.0.2", OS: "linux", Online: true},
			{Hostname: "leaver", IP: "100.64.0.3", OS: "linux", Online: true},
		},
	})

	// (1) 離開分母：每一個會講出「名冊上幾台」的畫面都要說 1，不是 2。
	for _, path := range []string{"/", "/reports/tickets"} {
		body := getPage(t, mux, path)
		// 兩個頁面用不同的措辭，所以兩個都列。
		if strings.Contains(body, "名冊上 2 台") || strings.Contains(body, "名冊 2 台") {
			t.Errorf("%s 的分母還是 2：\n%s", path, firstLines(body, 8))
		}
	}

	// (2) 不離開畫面：首頁上要找得到它，而且要標明它退役了。
	home := getPage(t, mux, "/")
	if strings.Contains(fleetTable(t, home), "leaver") {
		t.Error("退役的機器還在機隊表格上 —— 它應該在「已退役」那一段")
	}
	if !strings.Contains(home, "leaver") {
		t.Error("退役的機器從首頁上整個消失了 —— 那是這個產品存在要修的那個 bug")
	}
	if !strings.Contains(home, "已退役") {
		t.Error("首頁上沒有「已退役」這一段 —— 那台機器出現在畫面上卻沒有標籤")
	}
	// ⚠ 這一條是真正的重點：路由存在不等於人到得了。
	// 「取消退役」在單機頁上，而單機頁只從連結點得進去。
	if !strings.Contains(home, `href="/machines/`+gone.id+`"`) {
		t.Error("首頁上沒有連到那台退役機器的連結 —— 取消退役變成一扇沒有門的房間")
	}
	// (3) 它不准被講成「不在名冊裡」。它在名冊裡，它是退役的。
	//
	// ⚠ 實測那次它就是被這樣講的：綠燈 online、標題寫「不在名冊裡的機器」、
	// 底下建議去跑 ignore-peer（那是給從來沒納管過的機器用的）。三句話全錯。
	// ⚠ 範圍要切在那張表格的 </table> 上，不是「往後幾百個字」。
	// 第一版切 2000 個字元，結果撞進下面那個「兩邊講的不一樣」的表格，
	// 而那張表格裡出現 leaver 是**正確的** —— 測試就紅在對的行為上。
	// 一個範圍抓錯的斷言，跟一個抓到 bug 的斷言長得一模一樣。
	if i := strings.Index(home, "不在名冊裡的機器"); i >= 0 {
		seg := home[i:]
		if j := strings.Index(seg, "</table>"); j > 0 {
			seg = seg[:j]
		}
		if strings.Contains(seg, "leaver") {
			t.Errorf("退役的機器被歸類成「不在名冊裡」：\n%s", seg)
		}
	}
	// (4) 矛盾要攤出來：退役了，但 tailnet 說它還活著。
	if !strings.Contains(home, "兩邊講的不一樣") {
		t.Error("退役的機器在 tailnet 上還 online，畫面上卻沒有把這個矛盾講出來")
	}

	// 早報也是一個分母的出口。⚠ 它不是網頁，所以上面那個迴圈掃不到它。
	rep, err := h.buildReport(time.Now(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if strings.Contains(rep, "leaver") {
		t.Errorf("早報還在講退役的機器：\n%s", rep)
	}
	if !strings.Contains(rep, "/1 報到") {
		t.Errorf("早報的分母不是 1：\n%s", rep)
	}
}

// ---------------------------------------------------------------- helpers

type enrolled struct{ id, token string }

// enrollViaHTTP 走真的 POST /v1/enrollments，不走 store 的捷徑。
// ⚠ 用捷徑就測不到 handler 那一層，而 token 是在那一層被發出去的。
func enrollViaHTTP(t *testing.T, mux *http.ServeMux, st *store.Store, name string) enrolled {
	t.Helper()
	tok, err := st.CreateEnrollToken(name, time.Hour)
	if err != nil {
		t.Fatalf("enroll token: %v", err)
	}
	body, _ := json.Marshal(model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: name, OS: "linux", Arch: "amd64", UnixUser: "example-user",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/enrollments", bytes.NewReader(body))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll %s = %d：%s", name, rec.Code, rec.Body.String())
	}
	var resp model.EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode enroll resp: %v", err)
	}
	return enrolled{id: resp.MachineID, token: resp.AgentToken}
}

func postObservation(t *testing.T, mux *http.ServeMux, token string, want int) {
	t.Helper()
	now := time.Now().UTC()
	b, _ := json.Marshal(model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		Identity: model.Identity{Hostname: "h", OS: "linux", Arch: "amd64", UnixUser: "u"},
		CLITools: []model.CLITool{{
			Name: "openclaw", Present: true, Path: "/usr/bin/openclaw",
			VersionReported: "2026.6.1",
		}},
	})
	post(t, mux, "/v1/observations:batch", token, b, want)
}

func postCheckin(t *testing.T, mux *http.ServeMux, token string, want int) {
	t.Helper()
	b, _ := json.Marshal(model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: time.Now().UTC(),
		AgentVersion: "test", BootID: "boot-1", AgentSeq: 1,
	})
	post(t, mux, "/v1/checkins", token, b, want)
}

func post(t *testing.T, mux *http.ServeMux, path, token string, body []byte, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	mux.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Errorf("POST %s = %d，想要 %d：%s", path, rec.Code, want, rec.Body.String())
	}
}

func getPage(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d：%s", path, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// ⚠ 樣板執行錯誤不會變成 500，畫面會從出錯的地方斷掉 ——
	// 而一個截斷的畫面看起來就像「那台沒事」。
	if !strings.Contains(body, "</html>") {
		t.Fatalf("GET %s 的輸出被截斷了（樣板執行到一半失敗）", path)
	}
	return body
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// fleetTable 只回機隊那張表格的 HTML（<h2 id="machines"> 到下一個 <h2>）。
//
// ⚠ 這個 helper 是必要的，不是為了整潔：整頁比對分不出「在機隊表格裡」
// 跟「在已退役那一段裡」，而那正好是退役要分辨的兩件事。
// 上一版的測試就是拿整頁去比對，於是把「不在畫面上」當成了「不在分母裡」。
func fleetTable(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `id="machines"`)
	if i < 0 {
		t.Fatalf(`首頁上找不到 id="machines" —— 機隊表格整個不見了：\n%s`, firstLines(body, 8))
	}
	seg := body[i:]
	if j := strings.Index(seg, "<h2"); j > 0 {
		seg = seg[:j]
	}
	return seg
}
