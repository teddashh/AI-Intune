package tailnet

import (
	"testing"
	"time"
)

// 實機的名冊（2026-09-03）。⚠ 注意 hostname 跟 tailnet 上的名字對不起來，
// 而 sampleagent1 從未報到，所以它的 hostname 與 IP 都是空的。
func realRoster() []RosterEntry {
	return []RosterEntry{
		{MachineID: "m-samplehub1", DisplayName: "samplehub1", Hostname: "cnoderidge-ai1", TailscaleIP: "100.64.200.2"},
		{MachineID: "m-sampleagent1", DisplayName: "sampleagent1"},
		{MachineID: "m-sampleagent2", DisplayName: "sampleagent2", Hostname: "open-claw-vnic", TailscaleIP: "100.64.200.6"},
		{MachineID: "m-sampleagent3", DisplayName: "sampleagent3", Hostname: "instance-20260514-0131", TailscaleIP: "100.64.200.7"},
		{MachineID: "m-sampleagent4", DisplayName: "sampleagent4", Hostname: "fuhqsiem0", TailscaleIP: "100.64.200.5"},
	}
}

func realTailnet() Status {
	return Status{
		Available: true,
		Self:      Peer{Hostname: "cnoderidge-ai1", IP: "100.64.200.2", OS: "linux", Online: true},
		Peers: []Peer{
			{Hostname: "sampleagent1", IP: "100.64.200.8", OS: "linux", Online: true},
			{Hostname: "sampleagent2", IP: "100.64.200.6", OS: "linux", Online: true},
			{Hostname: "sampleagent3", IP: "100.64.200.7", OS: "linux", Online: true},
			{Hostname: "sampleagent4", IP: "100.64.200.5", OS: "linux", Online: true},
			{Hostname: "Sample-Workstation-01", IP: "100.64.200.4", OS: "windows", Online: true},
			{Hostname: "localhost", IP: "100.64.200.3", OS: "iOS", Online: false},
		},
	}
}

// hostname 對不起來就要靠 IP，而從未報到的機器連 IP 都沒有，只剩 display name。
func TestRosterMatchesEvenWhenHostnamesDisagree(t *testing.T) {
	r := Reconcile(realTailnet(), realRoster(), nil, nil)
	if !r.Available {
		t.Fatal(r.Unavailable)
	}
	got := map[string]bool{}
	for _, p := range r.Unenrolled {
		got[p.Hostname] = true
	}
	for _, should := range []string{"Sample-Workstation-01", "localhost"} {
		if !got[should] {
			t.Errorf("%s 不在名冊裡卻沒被列出來", should)
		}
	}
	for _, shouldNot := range []string{"sampleagent2", "sampleagent4", "cnoderidge-ai1", "sampleagent3"} {
		if got[shouldNot] {
			t.Errorf("%s 在名冊裡（IP 對得上）卻被當成名冊外的機器", shouldNot)
		}
	}
	// sampleagent1 的 hostname 與 IP 在名冊裡都是空的，只有 display name 能對上。
	if got["sampleagent1"] {
		t.Error("sampleagent1 在名冊裡（display name 對得上）卻被列成名冊外的機器 —— " +
			"一台從未報到的機器 hostname 與 IP 都是空的，那正是最需要對上的情況")
	}
}

// 這一格改變的是「下一步做什麼」。
func TestOnlineOnTailnetButSilentIsCalledOut(t *testing.T) {
	silent := map[string]bool{"m-sampleagent1": true} // Hub 說它從未報到
	r := Reconcile(realTailnet(), realRoster(), nil, silent)

	p, ok := r.OnlineButSilent["m-sampleagent1"]
	if !ok {
		t.Fatalf("sampleagent1 在 tailnet 上是 online 卻沒被點出來：%+v", r.OnlineButSilent)
	}
	if p.IP != "100.64.200.8" {
		t.Errorf("沒帶出可以連過去的位址：%+v", p)
	}
	if _, ok := r.OnlineButSilent["m-sampleagent2"]; ok {
		t.Error("sampleagent2 有在講話，不該出現在「活著但沉默」裡")
	}
}

// 被明確忽略的機器不佔畫面，但要數出來。
//
// Ignored peers remain counted, so unexpected peers are visible without listing known devices.
func TestIgnoredPeersAreCountedNotListed(t *testing.T) {
	ignored := map[string]bool{"sample-workstation-01": true, "localhost": true}
	r := Reconcile(realTailnet(), realRoster(), ignored, nil)

	if len(r.Unenrolled) != 0 {
		t.Errorf("忽略過的機器還是列出來了：%+v", r.Unenrolled)
	}
	if r.Ignored != 2 {
		t.Errorf("Ignored = %d, want 2 —— 忽略的台數要看得到，否則等於偷偷藏起來", r.Ignored)
	}
}

func TestStableIDIgnoreDoesNotHidePeersWithTheSameHostname(t *testing.T) {
	status := Status{Available: true, Self: Peer{StableID: "self", Hostname: "hub"}, Peers: []Peer{
		{StableID: "phone-1", Hostname: "localhost", IP: "100.64.0.2"},
		{StableID: "phone-2", Hostname: "localhost", IP: "100.64.0.3"},
	}}
	r := Reconcile(status, nil, map[string]bool{PeerIgnoreKey("phone-1"): true}, nil)
	if r.Ignored != 1 || len(r.Unenrolled) != 2 {
		t.Fatalf("reconcile=%+v", r)
	}
	seen := map[string]bool{}
	for _, peer := range r.Unenrolled {
		seen[peer.StableID] = true
	}
	if seen["phone-1"] || !seen["phone-2"] || !seen["self"] {
		t.Fatalf("unenrolled stable IDs=%v", seen)
	}
}

// ⚠ 問不到 tailscale 的時候，不准回一個看起來像「檢查過了，沒事」的空結果。
func TestNoTailscaleMeansUnknownNotClean(t *testing.T) {
	r := Reconcile(Status{Unavailable: "這台 Hub 上沒有 tailscale 指令"}, realRoster(), nil, nil)
	if r.Available {
		t.Fatal("問不到卻說 Available")
	}
	if r.Unavailable == "" {
		t.Error("沒有說明為什麼 —— 畫面上會變成一片安靜的空白，看的人會以為沒有名冊外的機器")
	}
	if len(r.Unenrolled) != 0 {
		t.Errorf("問不到卻列出了東西：%+v", r.Unenrolled)
	}
}

// TestRetiredMachineIsNotCalledUnenrolled
//
// ⚠⚠ 這是一個實機抓到的 bug 的測試。
//
// 原本 store.RosterForTailnet 把退役的機器從名冊裡濾掉，理由寫的是
// 「它已經離開分母，再出現在 tailnet 上是正常的」—— 意思是「不要吵」。
// 但濾掉的效果不是安靜，是**換一句錯的話**：對照不到的機器會掉進
// Unenrolled，而畫面上那一段的標題是「不在名冊裡的機器」。
//
// 實測（2026-09-03，退役 sampleagent1 兩秒）：首頁把 sampleagent1 列成「不在名冊裡」、
// 綠燈 online、還建議去跑 ignore-peer。它在名冊裡、它是人手動退役的、
// 而 ignore-peer 是給從來沒納管過的機器用的。三句話全錯。
//
// 沉默要靠說對的話達成，不是靠把資料抽掉。
func TestRetiredMachineIsNotCalledUnenrolled(t *testing.T) {
	roster := realRoster()
	for i := range roster {
		if roster[i].DisplayName == "sampleagent2" {
			roster[i].Retired = true
			roster[i].RetiredAt = time.Date(2026, 9, 3, 15, 17, 0, 0, time.UTC)
		}
	}
	// ⚠ silent 裡也放它：退役的機器當然收不到心跳。這個測試要確認
	// 它**不會**因此變成一個「agent 沒在報，快去修」的警報。
	r := Reconcile(realTailnet(), roster, nil, map[string]bool{"m-sampleagent2": true})

	for _, p := range r.Unenrolled {
		if p.Hostname == "sampleagent2" || p.IP == "100.64.200.6" {
			t.Errorf("退役的機器被歸類成「不在名冊裡」：%+v", p)
		}
	}
	if _, ok := r.OnlineButSilent["m-sampleagent2"]; ok {
		t.Error("退役的機器被列成「活著但 agent 沒在報」—— " +
			"那會產生一個要人去修的警報，而那裡沒有東西壞掉")
	}
	if len(r.RetiredButOnline) != 1 {
		t.Fatalf("RetiredButOnline = %+v，想要剛好 1 台", r.RetiredButOnline)
	}
	got := r.RetiredButOnline[0]
	if got.MachineID != "m-sampleagent2" || got.DisplayName != "sampleagent2" {
		t.Errorf("認錯機器了：%+v", got)
	}
	// ⚠ 退役時間要帶過來 —— 「五分鐘前退役」跟「三週前退役」
	// 要做的事不一樣，而畫面上只有這一個欄位能分辨。
	if got.RetiredAt.IsZero() {
		t.Error("沒有帶退役時間 —— 那就分不出「程序還沒做完」跟「退役按錯了機器」")
	}
	if !got.Peer.Online {
		t.Errorf("這一格的意思是「tailnet 說它還活著」，但 Online=false：%+v", got.Peer)
	}
}

// 退役而且 tailnet 上也看不到它 —— 那沒有矛盾，不要講話。
func TestRetiredAndOfflineIsNotAContradiction(t *testing.T) {
	roster := realRoster()
	status := realTailnet()
	for i := range roster {
		if roster[i].DisplayName == "sampleagent2" {
			roster[i].Retired = true
		}
	}
	for i := range status.Peers {
		if status.Peers[i].Hostname == "sampleagent2" {
			status.Peers[i].Online = false
		}
	}
	r := Reconcile(status, roster, nil, nil)
	if len(r.RetiredButOnline) != 0 {
		t.Errorf("退役又離線的機器不該出現在矛盾清單上：%+v", r.RetiredButOnline)
	}
	for _, p := range r.Unenrolled {
		if p.Hostname == "sampleagent2" {
			t.Error("退役又離線的機器被歸類成「不在名冊裡」")
		}
	}
}
