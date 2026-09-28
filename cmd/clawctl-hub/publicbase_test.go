package main

import (
	"strings"
	"testing"
)

// 早報那個連結是 PHASES.md「≤ 兩次點擊 + 一次 Connect」的第一次點擊。
//
// ⚠ 這組測試裡最重要的不是「對的時候會產出連結」，而是
// **錯的時候一定不產出連結、而且說得出理由**。
//
// 一個指向 127.0.0.1 的連結在 Hub 自己身上永遠測得通 —— 它只會在
// 手機上失敗，也就是唯一會有人點它的地方。這是「自證不算數」的
// 一個很具體的形狀：Hub 有資格說「我綁在這個位址上」，
// 沒有資格說「你連得到這個位址」。
func TestPublicBase(t *testing.T) {
	cases := []struct {
		name, env, listen, wantBase, wantWhy string
	}{
		{name: "tailnet 位址推得出來",
			listen: "100.64.200.2:8787", wantBase: "http://100.64.200.2:8787"},
		{name: "Tailscale IPv6 要有中括號",
			listen: "[fd7a:115c:a1e0::1234]:8787", wantBase: "http://[fd7a:115c:a1e0::1234]:8787"},
		{name: "主機名不能繞過 pinned Host",
			listen: "hub.example:8787", wantWhy: "不是 literal IP"},
		{name: "一般 ULA 不是 Tailscale IP",
			listen: "[fd00::1]:8787", wantWhy: "不是 Tailscale IP"},
		{name: "LAN IP 不是 Tailscale IP",
			listen: "192.168.1.20:8787", wantWhy: "不是 Tailscale IP"},
		{name: "Tailscale service IP 不是 node authority",
			listen: "100.100.100.100:8787", wantWhy: "不是 Tailscale node IP"},
		{name: "Tailscale Via range 不是 node authority",
			listen: "[fd7a:115c:a1e0:b1a::1]:8787", wantWhy: "不是 Tailscale node IP"},

		{name: "loopback 不給連結",
			listen: "127.0.0.1:8787", wantWhy: "loopback"},
		{name: "IPv6 loopback 也不給",
			listen: "[::1]:8787", wantWhy: "loopback"},
		{name: "0.0.0.0 推不出對外位址",
			listen: "0.0.0.0:8787", wantWhy: "所有介面"},
		{name: "省略主機也一樣",
			listen: ":8787", wantWhy: "所有介面"},
		{name: "看不懂就說看不懂",
			listen: "8787", wantWhy: "看不懂"},
		{name: "空的監聽位址也是看不懂",
			listen: "", wantWhy: "看不懂"},

		{name: "相同的環境變數只作 compatibility assertion",
			env: "http://100.64.200.2:8787/", listen: "100.64.200.2:8787",
			wantBase: "http://100.64.200.2:8787"},
		{name: "環境變數不能改成另一個 hostname",
			env: "https://hub.ts.net", listen: "100.64.200.2:8787",
			wantWhy: "不一致"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAWCTL_PUBLIC_URL", c.env)
			base, why := publicBase(c.listen)
			if base != c.wantBase {
				t.Errorf("base = %q，想要 %q", base, c.wantBase)
			}
			switch {
			case c.wantWhy == "" && why != "":
				t.Errorf("不該有理由，卻說了 %q", why)
			case c.wantWhy != "" && !strings.Contains(why, c.wantWhy):
				t.Errorf("理由 %q 裡沒有 %q", why, c.wantWhy)
			}
			// ⚠ 沒有 base 的時候一定要有理由。這一條比上面任何一條都重要：
			// 一個沒有理由地消失的連結，是沒有人會發現它壞掉的功能。
			if base == "" && why == "" {
				t.Error("沒有連結、也沒有理由 —— 這正好是不准出現的那個狀態")
			}
		})
	}
}
