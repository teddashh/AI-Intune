package tailnet

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const realSample = `{
  "Self": {"ID":"node-self","HostName":"cnoderidge-ai1","TailscaleIPs":["100.64.200.2","fd7a:115c:a1e0::d533:c833"],"OS":"linux","Online":true,"LastSeen":"0001-01-01T00:00:00Z"},
  "Peer": {
    "k1": {"ID":"node-sampleagent1","HostName":"sampleagent1","TailscaleIPs":["100.64.200.8"],"OS":"linux","Online":true,"LastSeen":"0001-01-01T00:00:00Z"},
    "k2": {"ID":"node-dell2","HostName":"SampleHub-Dell-02","TailscaleIPs":["100.64.200.1"],"OS":"windows","Online":false,"LastSeen":"2026-07-29T05:40:50Z"}
  }
}`

func fixed(b string, err error) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return []byte(b), err }
}

func TestParsesRealTailscaleOutput(t *testing.T) {
	s := queryWith(context.Background(), fixed(realSample, nil))
	if !s.Available {
		t.Fatalf("解析失敗：%s", s.Unavailable)
	}
	if s.Self.StableID != "node-self" || s.Self.Hostname != "cnoderidge-ai1" || s.Self.IP != "100.64.200.2" {
		t.Errorf("self 錯了：%+v", s.Self)
	}
	if len(s.Peers) != 2 {
		t.Fatalf("peers = %d, want 2", len(s.Peers))
	}
	for _, p := range s.Peers {
		if p.StableID == "" {
			t.Errorf("peer 缺少 stable ID：%+v", p)
		}
		if strings.Contains(p.IP, ":") {
			t.Errorf("%s 拿到 IPv6 位址 %s —— 畫面上沒人認得，而且 samplehub1 的 LAN 沒有 v6",
				p.Hostname, p.IP)
		}
		if p.Hostname == "sampleagent1" && !p.LastSeen.IsZero() {
			t.Errorf("online 的 peer 其 LastSeen 是 0001-01-01，不該被當成真的時間：%v", p.LastSeen)
		}
		if p.Hostname == "SampleHub-Dell-02" && p.LastSeen.Year() != 2026 {
			t.Errorf("offline 的 peer 應該留住 LastSeen，得到 %v", p.LastSeen)
		}
	}
}

// ⚠ 這是這個套件最重要的測試。
//
// 問不到 tailscale 的時候，回傳的必須是「沒有這個來源」，
// **不是「0 台在名冊外」**。這兩件事在畫面上長得幾乎一樣，意思差很多：
// 前者是「我不知道」，後者是「我確認過了，沒事」。
//
// 2026-09-03 §5.9 就是這樣來的：`clawctl-hub machines` 開錯資料庫、
// 建了一個空的、然後理直氣壯地說「名冊上 0 台」。
// 一個空的、乾淨的、沒有壞消息的答案，第一個要懷疑的是有沒有問對地方。
func TestCannotAskIsNotTheSameAsNothingFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context) ([]byte, error)
	}{
		{"沒裝 tailscale", fixed("", errors.New("exec: \"tailscale\": executable file not found in $PATH"))},
		{"輸出不是 JSON", fixed("Tailscale is stopped.", nil)},
		{"還沒登入", fixed(`{"Self":null,"Peer":null}`, nil)},
		{"tailscale 自己回報錯誤", fixed(`{"Error":"not logged in"}`, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := queryWith(context.Background(), tc.run)
			if s.Available {
				t.Fatalf("問不到卻回報 Available=true：%+v", s)
			}
			if len(s.Peers) != 0 {
				t.Errorf("問不到卻有 peers：%+v", s.Peers)
			}
			if strings.TrimSpace(s.Unavailable) == "" {
				t.Error("沒有說明為什麼問不到 —— 畫面上會變成一個安靜的空白，" +
					"看的人會以為「沒有名冊外的機器」")
			}
		})
	}
}

func TestUserDirectoryCountsOwnersIncludingSelfAndNamelessPeers(t *testing.T) {
	const sample = `{
	  "Self": {"ID":"node-self","HostName":"hub","UserID":1000000000000001,"Online":true,"OS":"linux","TailscaleIPs":["100.1.1.1"]},
	  "Peer": {
	    "a": {"ID":"node-a","HostName":"samplehub1","UserID":1000000000000001,"Online":true,"OS":"linux","TailscaleIPs":["100.1.1.2"]},
	    "b": {"ID":"node-b","HostName":"","UserID":7,"Online":true,"OS":"linux"},
	    "c": {"ID":"node-c","HostName":"other","UserID":99,"Online":false,"OS":"windows","TailscaleIPs":["100.2.2.2"]}
	  },
	  "User": {
	    "1000000000000001": {"ID":1000000000000001,"LoginName":"operator@example.com","DisplayName":"Sample Operator"},
	    "99": {"ID":99,"LoginName":"other@example.com","DisplayName":"Other"},
	    "7": {"ID":7,"LoginName":"nameless@example.com","DisplayName":"Nameless"},
	    "5": {"ID":5,"LoginName":"idle@example.com","DisplayName":"Idle"}
	  }
	}`
	status := queryWith(context.Background(), fixed(sample, nil))
	if !status.Available {
		t.Fatal(status.Unavailable)
	}
	if status.Self.UserID != "1000000000000001" || status.Self.UserLogin != "operator@example.com" {
		t.Fatalf("self user=%+v", status.Self)
	}
	if len(status.Peers) != 2 {
		t.Fatalf("nameless peer stayed on the machine list: %+v", status.Peers)
	}
	directory := Users(status)
	if !directory.Available || directory.Unavailable != "" {
		t.Fatalf("directory=%+v", directory)
	}
	if len(directory.Users) != 3 {
		t.Fatalf("users=%+v", directory.Users)
	}
	got := map[string]User{}
	for _, user := range directory.Users {
		got[user.UserID] = user
	}
	if _, ok := got["5"]; ok {
		t.Fatalf("user with no peer was listed: %+v", directory.Users)
	}
	owner := got["1000000000000001"]
	if owner.Login != "operator@example.com" || owner.DisplayName != "Sample Operator" || owner.PeerCount != 2 {
		t.Fatalf("self owner=%+v", owner)
	}
	if got["7"].Login != "nameless@example.com" || got["7"].PeerCount != 1 {
		t.Fatalf("nameless peer owner=%+v", got["7"])
	}
	if got["99"].PeerCount != 1 || got["99"].Login != "other@example.com" {
		t.Fatalf("other=%+v", got["99"])
	}
}

func TestUserDirectoryUnavailableIsNotAnEmptyDirectory(t *testing.T) {
	answered := queryWith(context.Background(), fixed(`{"Self":{"ID":"self","HostName":"hub","Online":true},"Peer":{}}`, nil))
	empty := Users(answered)
	if !empty.Available || empty.Unavailable != "" || len(empty.Users) != 0 {
		t.Fatalf("an answered tailnet with no owners is a real empty directory: %+v", empty)
	}
	for _, tc := range []struct {
		name string
		run  func(context.Context) ([]byte, error)
	}{
		{"沒裝 tailscale", fixed("", errors.New("exec: \"tailscale\": executable file not found in $PATH"))},
		{"輸出不是 JSON", fixed("Tailscale is stopped.", nil)},
		{"還沒登入", fixed(`{"Self":null,"Peer":null}`, nil)},
		{"tailscale 自己回報錯誤", fixed(`{"Error":"not logged in"}`, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := queryWith(context.Background(), tc.run)
			directory := Users(status)
			if directory.Available || len(directory.Users) != 0 || strings.TrimSpace(directory.Unavailable) == "" {
				t.Fatalf("讀不到被編成空名冊：status=%+v directory=%+v", status, directory)
			}
			if directory.Available == empty.Available && directory.Unavailable == empty.Unavailable {
				t.Fatalf("讀不到與問到的空名冊是同一個結果：%+v", directory)
			}
		})
	}
}

// 沒有名字的 peer 不能佔一格。
func TestNamelessPeersAreDropped(t *testing.T) {
	s := queryWith(context.Background(), fixed(
		`{"Self":{"HostName":"h"},"Peer":{"a":{"HostName":""},"b":null,"c":{"HostName":"real"}}}`, nil))
	if !s.Available {
		t.Fatal(s.Unavailable)
	}
	if len(s.Peers) != 1 || s.Peers[0].Hostname != "real" {
		t.Errorf("沒名字的 peer 應該被丟掉，得到 %+v", s.Peers)
	}
}
