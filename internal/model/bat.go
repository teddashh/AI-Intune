package model

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ConnectURL 回傳「人可以貼進瀏覽器的那個位址」，以及給不出來時的原因。
//
// ⚠⚠ 這個函式的重點是**它會拒絕給答案**。
//
// 機隊四台有兩台 --bind=localhost，那兩台不管填哪個 IP 都連不上。
// 對它們回傳一個看起來合理的網址，就是把「連不上」的成本從
// 「頁面上一句話」推遲到「人試了三次才發現」。
//
// tailscaleIP 是那台自己報的位址（不是名冊上報到當天記的那個）。
func ConnectURL(b BAT, tailscaleIP string) (url, why string) {
	switch {
	case b.Reason != "" && !b.Running:
		return "", b.Reason
	case !b.Running:
		return "", "沒有看到 bat-server 在跑"
	case b.Port == 0:
		return "", b.Reason
	}

	// 核心的表最有話語權：如果有一個非 loopback 的位址在聽，就用它。
	// ⚠ 這比 tailscaleIP 可信 —— 它是「真的綁上去的那個」。
	var loopbackOnly = true
	for _, a := range b.ListenAddrs {
		ip := net.ParseIP(a)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		loopbackOnly = false
		if ip.IsUnspecified() {
			// 0.0.0.0 / :: 代表「所有介面」，那時候要挑一個人連得到的，
			// 而唯一知道的就是它自己報的 tailnet 位址。
			continue
		}
		return "https://" + net.JoinHostPort(a, strconv.Itoa(b.Port)), ""
	}

	if len(b.ListenAddrs) > 0 && loopbackOnly {
		return "", fmt.Sprintf(
			"bat-server 只綁在 %s（argv 是 --bind=%s）—— 從別台機器連不進來，"+
				"要先改成對外綁定或者走 SSH port forward",
			strings.Join(b.ListenAddrs, "、"), firstNonEmpty(b.Bind, "?"))
	}
	if tailscaleIP == "" {
		return "", firstNonEmpty(b.Reason, "不知道這台的 Tailscale 位址")
	}
	if len(b.ListenAddrs) == 0 {
		return "", firstNonEmpty(b.Reason,
			fmt.Sprintf("核心的 socket 表上沒有東西在聽 %d", b.Port))
	}
	// 綁在所有介面上：用它自己報的 tailnet 位址。
	return "https://" + net.JoinHostPort(tailscaleIP, strconv.Itoa(b.Port)), ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
